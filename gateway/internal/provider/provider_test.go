package provider

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestMatchAllowlist(t *testing.T) {
	tv, ex := NewTavily(""), NewExa("")
	cases := []struct {
		p            Provider
		method, path string
		name         string
		ok           bool
	}{
		{tv, "POST", "/search", "search", true},
		{tv, "POST", "/search/", "search", true},
		{tv, "POST", "/extract", "extract", true},
		{tv, "POST", "/research", "research", true},
		{tv, "GET", "/research/req-123", "research_get", true},
		{tv, "GET", "/usage", "usage", true},
		{tv, "GET", "/search", "", false},
		{tv, "POST", "/admin/keys", "", false},
		{tv, "GET", "/research/a/b", "", false},
		{ex, "POST", "/search", "search", true},
		{ex, "POST", "/contents", "contents", true},
		{ex, "POST", "/research/v1", "research", true},
		{ex, "GET", "/research/v1/r_1", "research_get", true},
		{ex, "DELETE", "/search", "", false},
		{ex, "POST", "/websets/v0/x", "", false},
	}
	for _, c := range cases {
		op, ok := c.p.Match(c.method, c.path)
		if ok != c.ok || op.Name != c.name {
			t.Errorf("%s %s %s → %q %v, want %q %v", c.p.ID(), c.method, c.path, op.Name, ok, c.name, c.ok)
		}
	}
	op, _ := tv.Match("GET", "/research/req-123")
	if op.Task != TaskPoll || op.TaskID != "req-123" || !op.RetryAfterSend || op.Billable {
		t.Errorf("research_get = %+v", op)
	}
	op, _ = tv.Match("POST", "/research")
	if op.Task != TaskCreate || op.RetryAfterSend {
		t.Errorf("research create must not be resent after send: %+v", op)
	}
	if op, _ := tv.Match("POST", "/crawl"); op.RetryAfterSend {
		t.Error("crawl must not be resent after send (double billing)")
	}
}

func TestClassifyTavily(t *testing.T) {
	tv := NewTavily("")
	op, _ := tv.Match("POST", "/search")
	cases := []struct {
		status int
		h      http.Header
		body   string
		class  Class
		retry  bool
	}{
		{200, nil, `{"results":[]}`, ClassOK, false},
		{400, nil, `{"detail":{"error":"Query is missing"}}`, ClassClientError, false},
		{422, nil, `{"detail":[{"msg":"bad"}]}`, ClassClientError, false},
		{401, nil, `{"detail":{"error":"Unauthorized: missing or invalid API key."}}`, ClassKeyInvalid, true},
		{432, nil, `{"detail":{"error":"This request exceeds your plan's set usage limit."}}`, ClassKeyExhausted, true},
		{433, nil, `{"detail":{"error":"This request exceeds the pay-as-you-go limit."}}`, ClassKeyExhausted, true},
		{429, hdr("Retry-After", "42"), `{"detail":{"error":"rate limit"}}`, ClassRateLimited, true},
		{403, hdr("Content-Type", "text/html"), `<html><head><title>403 Forbidden</title></head><body><center>awselb/2.0</center></body></html>`, ClassEdgeBlocked, true},
		{403, nil, `<!DOCTYPE html><html>blocked</html>`, ClassEdgeBlocked, true},
		{403, hdr("Content-Type", "application/json"), `{"detail":{"error":"Forbidden"}}`, ClassKeyForbidden, true},
		{502, nil, `bad gateway`, ClassServerError, true},
	}
	for _, c := range cases {
		h := c.h
		if h == nil {
			h = http.Header{}
		}
		o := tv.Classify(op, c.status, h, []byte(c.body))
		if o.Class != c.class || o.Retry != c.retry || o.Status != c.status {
			t.Errorf("%d %s → %v retry=%v, want %v retry=%v", c.status, c.body, o.Class, o.Retry, c.class, c.retry)
		}
	}
	o := tv.Classify(op, 429, hdr("Retry-After", "42"), nil)
	if o.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %v", o.RetryAfter)
	}
	o = tv.Classify(op, 432, http.Header{}, []byte(`{"detail":{"error":"plan limit"}}`))
	if o.Reason != "plan limit" {
		t.Errorf("reason = %q", o.Reason)
	}
}

func TestClassifyExa(t *testing.T) {
	ex := NewExa("")
	op, _ := ex.Match("POST", "/search")
	for status, want := range map[int]Class{
		200: ClassOK, 400: ClassClientError, 401: ClassKeyInvalid, 402: ClassKeyExhausted,
		404: ClassClientError, 429: ClassRateLimited, 500: ClassServerError, 503: ClassServerError,
	} {
		if o := ex.Classify(op, status, http.Header{}, []byte(`{"error":"x","tag":"NO_MORE_CREDITS"}`)); o.Class != want {
			t.Errorf("%d → %v, want %v", status, o.Class, want)
		}
	}
	if o := ex.Classify(op, 402, http.Header{}, []byte(`{"error":"You have run out of credits","tag":"NO_MORE_CREDITS"}`)); o.Reason != "You have run out of credits" {
		t.Errorf("reason = %q", o.Reason)
	}
}

func TestTavilyRewriteAndCost(t *testing.T) {
	tv := NewTavily("")
	op, _ := tv.Match("POST", "/search")
	in := `{"query":"golang","api_key":"sw_client_token","search_depth":"advanced","max_results":3}`
	out, injected, err := tv.RewriteRequest(op, []byte(in), true)
	if err != nil || !injected {
		t.Fatalf("rewrite: %v %v", injected, err)
	}
	if strings.Contains(string(out), "sw_client_token") || !strings.Contains(string(out), `"include_usage":true`) ||
		!strings.HasPrefix(string(out), `{"query":"golang",`) {
		t.Fatalf("request = %s (client token must be gone, field order kept)", out)
	}
	// Client already asked for usage: nothing injected, nothing stripped.
	out2, injected2, _ := tv.RewriteRequest(op, []byte(`{"query":"x","include_usage":true}`), true)
	if injected2 || string(out2) != `{"query":"x","include_usage":true}` {
		t.Fatalf("explicit include_usage: %s %v", out2, injected2)
	}
	// Precise billing off: body only loses credentials.
	out3, injected3, _ := tv.RewriteRequest(op, []byte(`{"query":"x"}`), false)
	if injected3 || string(out3) != `{"query":"x"}` {
		t.Fatalf("billing off: %s", out3)
	}

	resp := []byte(`{"query":"golang","results":[],"usage":{"credits":2},"request_id":"r1"}`)
	c, ok := tv.ActualCost(op, resp)
	if !ok || c.Credits != 2 || c.Estimated {
		t.Fatalf("cost = %+v %v", c, ok)
	}
	stripped := tv.RewriteResponse(op, resp, true)
	if strings.Contains(string(stripped), "usage") || !strings.Contains(string(stripped), `"request_id":"r1"`) {
		t.Fatalf("response = %s", stripped)
	}
	if string(tv.RewriteResponse(op, resp, false)) != string(resp) {
		t.Fatal("usage the client asked for must be kept")
	}
	// Non-JSON body passes through untouched.
	if out, inj, _ := tv.RewriteRequest(op, []byte("not json"), true); string(out) != "not json" || inj {
		t.Fatal("non-JSON body altered")
	}
}

func TestTavilyEstimates(t *testing.T) {
	tv := NewTavily("")
	est := func(path, body string) int64 {
		op, _ := tv.Match("POST", path)
		return tv.EstimateCost(op, []byte(body)).Credits
	}
	cases := []struct {
		path, body string
		want       int64
	}{
		{"/search", `{"query":"a"}`, 1},
		{"/search", `{"query":"a","search_depth":"advanced"}`, 2},
		{"/extract", `{"urls":["a","b","c","d","e","f"]}`, 2},
		{"/extract", `{"urls":"a","extract_depth":"advanced"}`, 2},
		{"/map", `{"url":"x","limit":25}`, 3},
		{"/map", `{"url":"x","limit":25,"instructions":"docs only"}`, 6},
		{"/crawl", `{"url":"x","limit":10}`, 3},
		{"/research", `{"input":"x","model":"mini"}`, 4},
		{"/research", `{"input":"x","model":"pro"}`, 15},
		{"/research", `{"input":"x"}`, 4},
	}
	for _, c := range cases {
		if got := est(c.path, c.body); got != c.want {
			t.Errorf("%s %s → %d, want %d", c.path, c.body, got, c.want)
		}
	}
}

func TestExaCost(t *testing.T) {
	ex := NewExa("")
	op, _ := ex.Match("POST", "/search")
	c, ok := ex.ActualCost(op, []byte(`{"results":[],"costDollars":{"total":0.007,"search":{"neural":0.005}}}`))
	if !ok || c.MicroUSD != 7000 {
		t.Fatalf("cost = %+v", c)
	}
	if _, ok := ex.ActualCost(op, []byte(`{"results":[]}`)); ok {
		t.Fatal("missing costDollars must report !ok")
	}
	if e := ex.EstimateCost(op, []byte(`{"query":"x","numResults":5,"contents":{"text":true}}`)); e.MicroUSD != 12000 || !e.Estimated {
		t.Fatalf("estimate = %+v", e)
	}
}

func TestTaskIDsAndPreview(t *testing.T) {
	tv, ex := NewTavily(""), NewExa("")
	op, _ := tv.Match("POST", "/research")
	if id := tv.TaskIDFromResponse(op, []byte(`{"request_id":"req_9","status":"pending"}`)); id != "req_9" {
		t.Errorf("tavily task id = %q", id)
	}
	eop, _ := ex.Match("POST", "/research/v1")
	if id := ex.TaskIDFromResponse(eop, []byte(`{"researchId":"r_7"}`)); id != "r_7" {
		t.Errorf("exa task id = %q", id)
	}

	sop, _ := tv.Match("POST", "/search")
	long := `{"query":"  golang   concurrency patterns 并发模式 with many extra words to exceed the limit  "}`
	if got := tv.QueryPreview(sop, []byte(long), 20); got != "golang concurrency p" {
		t.Errorf("preview = %q", got)
	}
	if got := tv.QueryPreview(sop, []byte(`{"query":"中文搜索词测试"}`), 3); got != "中文搜" {
		t.Errorf("CJK preview = %q (must cut by characters)", got)
	}
	xop, _ := tv.Match("POST", "/extract")
	if got := tv.QueryPreview(xop, []byte(`{"urls":["https://user:pw@example.com/a?token=secret#frag","https://b.example"]}`), 40); got != "https://example.com/a +1" {
		t.Errorf("url preview = %q (query, fragment and credentials must be dropped)", got)
	}
	if tv.QueryPreview(sop, []byte(`{"query":"x"}`), 0) != "" {
		t.Error("n=0 must disable previews")
	}
	cop, _ := ex.Match("POST", "/contents")
	if got := ex.QueryPreview(cop, []byte(`{"ids":["https://a.example/x?y=1"]}`), 40); got != "https://a.example/x" {
		t.Errorf("exa contents preview = %q", got)
	}
}

func TestTavilyParseUsage(t *testing.T) {
	tv := NewTavily("")
	cases := []struct {
		name                   string
		body                   string
		used, limit, remaining int64
	}{
		// Key without its own limit: it may spend what the account has left (shared
		// with the account's other keys), but its own usage is what it used.
		{"account plan", `{"key":{"usage":120,"limit":null},"account":{"current_plan":"Researcher","plan_usage":150,"plan_limit":1000}}`, 120, 970, 850},
		{"key limit tighter", `{"key":{"usage":30,"limit":100},"account":{"plan_usage":900,"plan_limit":1000}}`, 30, 100, 70},
		{"account tighter than key limit", `{"key":{"usage":30,"limit":500},"account":{"plan_usage":950,"plan_limit":1000}}`, 30, 80, 50},
		{"plan used up, pay-as-you-go left", `{"key":{"usage":10,"limit":null},"account":{"plan_usage":1000,"plan_limit":1000,"paygo_usage":20,"paygo_limit":100}}`, 10, 90, 80},
		{"plan counter past its limit", `{"key":{"usage":10},"account":{"plan_usage":1020,"plan_limit":1000,"paygo_usage":20,"paygo_limit":100}}`, 10, 90, 80},
		{"pay-as-you-go off (null)", `{"key":{"usage":5,"limit":null},"account":{"plan_usage":1000,"plan_limit":1000,"paygo_usage":0,"paygo_limit":null}}`, 5, 5, 0},
		{"no per-key usage", `{"account":{"plan_usage":150,"plan_limit":1000,"paygo_usage":0}}`, 150, 1000, 850},
	}
	for _, c := range cases {
		q, err := tv.ParseUsage([]byte(c.body))
		if err != nil || q.Used == nil || q.Limit == nil || q.Remaining == nil {
			t.Fatalf("%s: %+v %v", c.name, q, err)
		}
		if *q.Used != c.used || *q.Limit != c.limit || *q.Remaining != c.remaining {
			t.Errorf("%s: used/limit/remaining = %d/%d/%d, want %d/%d/%d", c.name, *q.Used, *q.Limit, *q.Remaining, c.used, c.limit, c.remaining)
		}
	}
	// No limit anywhere (e.g. an enterprise account): usage known, balance unknown.
	q, err := tv.ParseUsage([]byte(`{"key":{"usage":7,"limit":null},"account":{"current_plan":"Enterprise","plan_usage":7,"plan_limit":null}}`))
	if err != nil || *q.Used != 7 || q.Limit != nil || q.Remaining != nil || q.Plan != "Enterprise" {
		t.Fatalf("unlimited: %+v %v", q, err)
	}
	for _, bad := range []string{`<html>`, `{"detail":{"error":"x"}}`, `[]`} {
		if _, err := tv.ParseUsage([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := NewExa("").ParseUsage(nil); err != ErrUnsupported {
		t.Fatal("exa usage must be unsupported")
	}
}

// Exa error tags (exa.ai/docs/admin/error-codes): moderation is the request's
// fault, team-level balance and blocks apply to the whole team.
func TestExaErrorTags(t *testing.T) {
	ex := NewExa("")
	op, _ := ex.Match(http.MethodPost, "/search")
	cases := []struct {
		status      int
		tag         string
		class       Class
		retry, team bool
	}{
		{403, "PROHIBITED_CONTENT", ClassClientError, false, false},
		{403, "CONTENT_FILTER_ERROR", ClassClientError, false, false},
		{403, "FEATURE_DISABLED", ClassClientError, true, false},
		{403, "TEAM_BLOCKED", ClassKeyInvalid, true, true},
		{403, "SOMETHING_NEW", ClassKeyForbidden, true, false},
		{402, "NO_MORE_CREDITS", ClassKeyExhausted, true, true},
		{402, "TEAM_BUDGET_EXCEEDED", ClassKeyExhausted, true, true},
		{402, "API_KEY_BUDGET_EXCEEDED", ClassKeyExhausted, true, false},
		{402, "X402_PAYMENT_REQUIRED", ClassKeyInvalid, true, false},
		{401, "INVALID_API_KEY", ClassKeyInvalid, true, false},
	}
	for _, c := range cases {
		body := `{"requestId":"r","error":"e","tag":"` + c.tag + `"}`
		o := ex.Classify(op, c.status, http.Header{"Content-Type": {"application/json"}}, []byte(body))
		if o.Class != c.class || o.Retry != c.retry || o.TeamWide != c.team {
			t.Errorf("%d %s: class=%v retry=%v team=%v, want %v %v %v", c.status, c.tag, o.Class, o.Retry, o.TeamWide, c.class, c.retry, c.team)
		}
	}
}

func TestParseRetryAfterAndRedact(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	if d := ParseRetryAfter("120", now); d != 2*time.Minute {
		t.Errorf("seconds: %v", d)
	}
	if d := ParseRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); d != 90*time.Second {
		t.Errorf("date: %v", d)
	}
	if ParseRetryAfter("soon", now) != 0 || ParseRetryAfter("", now) != 0 {
		t.Error("invalid values must be 0")
	}
	if got := RedactSecret("bad key tvly-abc123456", "tvly-abc123456"); got != "bad key [key]" {
		t.Errorf("redact = %q", got)
	}
}

func TestAuthorize(t *testing.T) {
	h := http.Header{}
	NewTavily("").Authorize(h, "tvly-k")
	NewExa("").Authorize(h, "exa-k")
	if h.Get("Authorization") != "Bearer tvly-k" || h.Get("x-api-key") != "exa-k" {
		t.Fatalf("headers = %v", h)
	}
}

// Responses captured from the real APIs on 2026-11-09 with invalid or missing keys
// (no secrets involved). They pin the classification to what upstream really sends.
func TestRealErrorSamples(t *testing.T) {
	tv, ex := NewTavily(""), NewExa("")
	top, _ := tv.Match("POST", "/search")
	eop, _ := ex.Match("POST", "/search")
	json := hdr("Content-Type", "application/json")
	cases := []struct {
		name   string
		p      Provider
		op     Operation
		status int
		body   string
		class  Class
		reason string
	}{
		{"tavily invalid key", tv, top, 401, `{"detail":{"error":"Unauthorized: missing or invalid API key."}}`, ClassKeyInvalid, "Unauthorized: missing or invalid API key."},
		{"tavily pretty-printed body", tv, top, 401, "{\n    \"detail\": {\n        \"error\": \"Unauthorized: missing or invalid API key.\"\n    }\n}\n", ClassKeyInvalid, "Unauthorized: missing or invalid API key."},
		{"exa invalid key", ex, eop, 401, `{"requestId":"98441fba660907c2ead0cc29e9e5ccca","error":"Invalid API key. Provide a valid key using 'Authorization: Bearer <key>' or 'x-api-key: <key>'. Create a key at https://dashboard.exa.ai/api-keys","tag":"INVALID_API_KEY"}`, ClassKeyInvalid, ""},
		{"exa no key (x402)", ex, eop, 402, `{"requestId":"edae589575a066e775a279db4b05ae96","error":"Payment required to access this resource","tag":"X402_PAYMENT_REQUIRED","x402Version":2}`, ClassKeyInvalid, ""},
		{"exa out of credits", ex, eop, 402, `{"requestId":"x","error":"You have run out of credits","tag":"NO_MORE_CREDITS"}`, ClassKeyExhausted, "You have run out of credits"},
	}
	for _, c := range cases {
		o := c.p.Classify(c.op, c.status, json, []byte(c.body))
		if o.Class != c.class || !o.Retry {
			t.Errorf("%s: %v retry=%v, want %v", c.name, o.Class, o.Retry, c.class)
		}
		if c.reason != "" && o.Reason != c.reason {
			t.Errorf("%s: reason %q, want %q", c.name, o.Reason, c.reason)
		}
	}
}

// Shape of a real GET /usage answer for a free key (2026-11-09; values zeroed by upstream lag).
func TestTavilyUsageRealShape(t *testing.T) {
	body := `{"key":{"usage":0,"limit":null,"search_usage":0,"extract_usage":0,"crawl_usage":0,"map_usage":0,"research_usage":0},` +
		`"account":{"current_plan":"Researcher","plan_usage":0,"plan_limit":1000,"paygo_usage":0,"paygo_limit":null,` +
		`"search_usage":0,"extract_usage":0,"crawl_usage":0,"map_usage":0,"research_usage":0}}`
	q, err := NewTavily("").ParseUsage([]byte(body))
	if err != nil || q.Plan != "Researcher" || *q.Limit != 1000 || *q.Used != 0 || *q.Remaining != 1000 {
		t.Fatalf("quota = %+v %v", q, err)
	}
}
