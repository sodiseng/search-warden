package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDocumentedRateLimits(t *testing.T) {
	tv, ex := NewTavily(""), NewExa("")
	limits := func(p Provider, method, path, body, secret string, override float64) []Limit {
		op, ok := p.Match(method, path)
		if !ok {
			t.Fatalf("%s %s not allowed", method, path)
		}
		return p.Limits(op, []byte(body), secret, override)
	}
	check := func(name string, got []Limit, want ...Limit) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %+v, want %+v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: %+v, want %+v", name, got[i], want[i])
			}
		}
	}
	dev, prod := "tvly-dev-abc", "tvly-prod-abc"
	check("tavily dev search", limits(tv, "POST", "/search", "{}", dev, 0), Limit{"default", 100, time.Minute})
	check("tavily prod search", limits(tv, "POST", "/search", "{}", prod, 0), Limit{"default", 1000, time.Minute})
	check("tavily old-format key", limits(tv, "POST", "/search", "{}", "tvly-abc", 0), Limit{"default", 100, time.Minute})
	check("tavily override", limits(tv, "POST", "/search", "{}", "tvly-abc", 1000), Limit{"default", 1000, time.Minute})
	check("tavily crawl", limits(tv, "POST", "/crawl", "{}", prod, 0), Limit{"crawl", 100, time.Minute})
	check("tavily research create", limits(tv, "POST", "/research", "{}", prod, 0), Limit{"research", 20, time.Minute})
	check("tavily research poll", limits(tv, "GET", "/research/r1", "", dev, 0), Limit{"default", 100, time.Minute})

	check("exa search", limits(ex, "POST", "/search", `{"query":"x"}`, "k", 0), Limit{"search", 10, time.Second})
	check("exa deep search", limits(ex, "POST", "/search", `{"type":"deep"}`, "k", 0), Limit{"search", 10, time.Second}, Limit{"search_deep", 5, time.Second})
	check("exa 25 QPS team", limits(ex, "POST", "/search", `{}`, "k", 25), Limit{"search", 25, time.Second})
	check("exa answer shares search", limits(ex, "POST", "/answer", `{}`, "k", 0), Limit{"search", 10, time.Second})
	check("exa contents", limits(ex, "POST", "/contents", `{}`, "k", 25), Limit{"contents", 100, time.Second})
	check("exa research poll", limits(ex, "GET", "/research/v1/r1", "", "k", 0))
}

func TestRateLimitCooldownPolicies(t *testing.T) {
	tv, ex := NewTavily(""), NewExa("")
	top, _ := tv.Match("POST", "/search")
	eop, _ := ex.Match("POST", "/search")
	if o := tv.Classify(top, 429, http.Header{}, nil); o.CooldownBase != time.Minute || o.TeamWide {
		t.Errorf("tavily 429 = %+v", o)
	}
	if o := ex.Classify(eop, 429, http.Header{}, nil); o.CooldownBase != 2*time.Second || !o.TeamWide {
		t.Errorf("exa 429 = %+v", o)
	}
}

func TestTavilyFlexTierIsNotTheKeysFault(t *testing.T) {
	tv := NewTavily("")
	op, _ := tv.Match("POST", "/search")
	body := `{"detail":{"error":"Live content fetching is temporarily unavailable on your plan due to high demand."}}`
	o := tv.Classify(op, 434, hdr("Content-Type", "application/json"), []byte(body))
	if o.Class != ClassUpstreamBusy || o.Retry || !strings.HasPrefix(o.Reason, "Live content") {
		t.Fatalf("434 = %+v", o)
	}
}

func TestExaAgentRunRoutes(t *testing.T) {
	ex := NewExa("")
	create, ok := ex.Match(http.MethodPost, "/agent/runs")
	if !ok || create.Task != TaskCreate || create.RetryAfterSend || !create.Billable {
		t.Fatalf("create = %+v", create)
	}
	get, ok := ex.Match(http.MethodGet, "/agent/runs/agent_run_1")
	if !ok || get.Task != TaskPoll || get.TaskID != "agent_run_1" || !get.RetryAfterSend {
		t.Fatalf("get = %+v", get)
	}
	if c, ok := ex.Match(http.MethodPost, "/agent/runs/agent_run_1/cancel"); !ok || c.TaskID != "agent_run_1" {
		t.Fatalf("cancel = %+v", c)
	}
	if _, ok := ex.Match(http.MethodGet, "/agent/runs/a/b"); ok {
		t.Fatal("unknown agent path allowed")
	}
	// A run is billed once, from the poll that sees it end; starting one costs nothing yet.
	if c, ok := ex.ActualCost(get, []byte(`{"status":"running","costDollars":{"total":0.1}}`)); ok || c.MicroUSD != 0 {
		t.Fatalf("running run billed: %+v", c)
	}
	if c, ok := ex.ActualCost(get, []byte(`{"status":"completed","costDollars":{"total":0.25}}`)); !ok || c.MicroUSD != 250_000 {
		t.Fatalf("finished run = %+v %v", c, ok)
	}
	if c := ex.EstimateCost(create, nil); c.MicroUSD != 0 {
		t.Fatalf("create estimate = %+v", c)
	}
	// 5 QPS per team, and a start counts twice.
	if l := ex.Limits(create, nil, "", 0); len(l) != 1 || l[0].Count != 2.5 || ex.Limits(get, nil, "", 0) != nil {
		t.Fatalf("limits = %+v", l)
	}
	if p := ex.QueryPreview(create, []byte(`{"query":"find go releases"}`), 40); p != "find go releases" {
		t.Fatalf("preview = %q", p)
	}
}

// Answers recorded live on 2026-11-10.
func TestExaProbe(t *testing.T) {
	ex := NewExa("https://api.exa.ai")
	req, err := ex.ProbeRequest(context.Background(), "k")
	if err != nil || req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/agent/runs/"+probeRunID) || req.Header.Get("x-api-key") != "k" {
		t.Fatalf("request = %v %v", req, err)
	}
	cases := []struct {
		status       int
		body         string
		valid, known bool
	}{
		{404, `{"statusCode":404,"error":{"type":"NOT_FOUND","code":"RUN_NOT_FOUND","message":"Run not found."}}`, true, true},
		{401, `{"error":"Invalid API key.","tag":"INVALID_API_KEY"}`, false, true},
		{404, `<html>not found</html>`, false, false},
		{503, `{}`, false, false},
	}
	for _, c := range cases {
		if v, k := ex.ProbeValid(c.status, []byte(c.body)); v != c.valid || k != c.known {
			t.Errorf("%d %s: valid=%v known=%v", c.status, c.body, v, k)
		}
	}
}
