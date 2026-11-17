package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TestTavilyValidationMatchesOfficial replays every argument set recorded from
// the official remote server (testdata/golden/tavily-10-validation.json): what
// it rejected must be rejected here with the same JSON-RPC error, without an
// upstream call; what it accepted must reach the API.
func TestTavilyValidationMatchesOfficial(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "golden", "tavily-10-validation.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, fixtureOpts{})
	for _, route := range []string{"POST /search", "POST /extract", "POST /map", "POST /crawl"} {
		f.up.always(route, 200, `{"results":[]}`)
	}
	f.up.always("POST /research", 400, `{"detail":{"error":"Input task is invalid."}}`)
	rejected, accepted := 0, 0
	for i, c := range gjson.GetBytes(b, "cases").Array() {
		tool, args := c.Get("tool").String(), c.Get("arguments").Raw
		f.up.mu.Lock()
		before := len(f.up.reqs)
		f.up.mu.Unlock()
		r := f.post("/tavily/mcp", rpc(100+i, "tools/call", `{"name":"`+tool+`","arguments":`+args+`}`), map[string]string{"MCP-Protocol-Version": "2025-11-25"})
		got := r.final()
		f.up.mu.Lock()
		calls := len(f.up.reqs) - before
		f.up.mu.Unlock()
		if want := c.Get("error"); want.Exists() {
			rejected++
			if r.status != 200 || got.Get("error.code").Int() != want.Get("code").Int() || got.Get("error.message").String() != want.Get("message").String() {
				t.Errorf("%s %s\n got %d %s\nwant %s", tool, args, r.status, got.Get("error").Raw, want.Raw)
			}
			if got.Get("result").Exists() || calls != 0 {
				t.Errorf("%s %s: a rejected call produced a result or reached upstream (%d calls)", tool, args, calls)
			}
			continue
		}
		accepted++
		if got.Get("error").Exists() || !got.Get("result").Exists() || calls != 1 {
			t.Errorf("%s %s: the official server accepts this; got %s (%d upstream calls)", tool, args, got.Raw, calls)
		}
	}
	if rejected < 40 || accepted < 10 {
		t.Fatalf("golden file looks truncated: %d rejected, %d accepted", rejected, accepted)
	}
}

// What the official server sends to the API after validation, read from the
// bodies the API echoes in its error answers (2026-11-10).
func TestTavilyValidatedBodies(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	for _, route := range []string{"POST /search", "POST /extract", "POST /map"} {
		f.up.always(route, 200, `{"results":[]}`)
	}
	for _, c := range []struct{ tool, route, args, want string }{
		{"tavily_search", "POST /search", `{"query":"q","max_results":" 3 ","include_images":"NO","include_favicon":1,"exact_match":0.0,"time_range":null}`,
			`{"query":"q","max_results":3,"include_images":false,"include_favicon":true,"exact_match":false,"include_usage":true}`},
		{"tavily_search", "POST /search", `{"query":"q","max_results":true,"country":"","include_domains":[],"exclude_domains":["a.com"]}`,
			`{"query":"q","max_results":1,"exclude_domains":["a.com"],"include_usage":true}`},
		{"tavily_search", "POST /search", `{"query":"","max_results":1.0}`, `{"max_results":1,"include_usage":true}`}, // an empty query reaches the API as a missing one
		{"tavily_extract", "POST /extract", `{"urls":[""],"query":"","include_usage":null}`, `{"urls":[""],"query":"","include_usage":true}`},
		{"tavily_map", "POST /map", `{"url":"","limit":"2","select_paths":[]}`, `{"url":"","limit":2,"select_paths":[],"include_usage":true}`},
	} {
		f.call("/tavily/mcp", c.tool, c.args, nil)
		if got := f.up.last(t, c.route).body; got != c.want {
			t.Errorf("%s %s\n sent %s\n want %s", c.tool, c.args, got, c.want)
		}
	}
}

// The research result of the official server carries auth_mode like every
// other tool (recorded 2026-11-10); the API's own answer does not.
func TestTavilyResearchMatchesOfficial(t *testing.T) {
	args, want := golden(t, "tavily-09-tavily_research.json")
	api, err := sjson.Delete(want.Get("structuredContent").Raw, "auth_mode")
	if err != nil || !want.Get("structuredContent.auth_mode").Exists() {
		t.Fatalf("golden sample has no auth_mode (%v)", err)
	}
	id := want.Get("structuredContent.request_id").String()
	f := newFixture(t, fixtureOpts{})
	f.up.always("POST /research", 200, `{"request_id":"`+id+`","status":"pending"}`)
	f.up.always("GET /research/"+id, 200, api)
	got := f.call("/tavily/mcp", "tavily_research", args.Raw, nil)
	if text(got) != text(want) || got.Get("structuredContent").Raw != text(want) || got.Get("isError").Raw != "false" {
		t.Fatalf("research result differs from the official server:\n got %.300s\nwant %.300s", got.Raw, want.Raw)
	}
}

// An agent run reports its price from the moment it is created; the gateway
// charges it once, whichever answer shows the run ended (seen live 2026-11-10:
// one $0.012 run was booked as $0.024).
func TestExaAgentRunIsChargedOnce(t *testing.T) {
	const cost = `"usage":{"agentComputeUnits":0,"searches":3,"emails":0,"phoneNumbers":0},"costDollars":{"total":0.012,"agentCompute":0,"search":0.012,"emails":0,"phoneNumbers":0}`
	total := func(f *fixture) (micro int64, rows int) {
		for _, l := range f.rec.all() {
			micro += l.CostMicroUSD
			rows++
		}
		return
	}

	// Created "running" with the price already set, finished at the first poll.
	f := newFixture(t, fixtureOpts{})
	f.up.always("POST /agent/runs", 200, `{"id":"agent_run_1","status":"running","output":null,`+cost+`}`)
	f.up.always("GET /agent/runs/agent_run_1", 200, `{"id":"agent_run_1","status":"completed","stopReason":"schema_satisfied","output":{"text":"Paris"},`+cost+`}`)
	got := f.call("/exa/mcp", "agent_run", `{"query":"capital of France","effort":"minimal"}`, nil)
	if got.Get("structuredContent.status").String() != "completed" || got.Get("structuredContent.costDollars.total").Float() != 0.012 {
		t.Fatalf("result = %s", got.Raw)
	}
	f.call("/exa/mcp", "agent_run", `{"runId":"agent_run_1"}`, nil) // asking again is free
	if micro, rows := total(f); micro != 12000 {
		t.Errorf("one $0.012 run was booked as %d micro-dollars in %d log rows: %+v", micro, rows, f.rec.all())
	}
	for _, v := range f.pool.Views() {
		if v.Provider == "exa" && v.Used != nil && *v.Used != 12000 {
			t.Errorf("key %s used = %d micro-dollars, want 12000", v.Label, *v.Used)
		}
	}

	// Finished already in the answer to the create call: charged there, never again.
	g := newFixture(t, fixtureOpts{})
	g.up.always("POST /agent/runs", 200, `{"id":"agent_run_2","status":"completed","output":{"text":"Tokyo"},`+cost+`}`)
	g.up.always("GET /agent/runs/agent_run_2", 200, `{"id":"agent_run_2","status":"completed","output":{"text":"Tokyo"},`+cost+`}`)
	g.call("/exa/mcp", "agent_run", `{"query":"capital of Japan","effort":"minimal"}`, nil)
	g.call("/exa/mcp", "agent_run", `{"runId":"agent_run_2"}`, nil)
	if micro, _ := total(g); micro != 12000 {
		t.Errorf("a run that ended at creation was booked as %d micro-dollars: %+v", micro, g.rec.all())
	}
}
