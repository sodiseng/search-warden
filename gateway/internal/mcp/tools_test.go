package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
)

// golden loads a result recorded from an official remote MCP server (testdata/golden).
func golden(t *testing.T, name string) (args, res gjson.Result) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	g := gjson.ParseBytes(b)
	msgs := g.Get("messages").Array()
	return g.Get("arguments"), msgs[len(msgs)-1].Get("result")
}

func text(r gjson.Result) string { return r.Get("content.0.text").String() }

// ---- Tavily ---------------------------------------------------------------------

func TestTavilyOutputMatchesOfficial(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	for _, c := range []struct{ file, route string }{
		{"tavily-00-tavily_search.json", "POST /search"},
		{"tavily-01-tavily_extract.json", "POST /extract"},
		{"tavily-02-tavily_map.json", "POST /map"},
		{"tavily-03-tavily_crawl.json", "POST /crawl"},
	} {
		args, want := golden(t, c.file)
		api, err := sjson.Delete(want.Get("structuredContent").Raw, "auth_mode") // what the API answered: auth_mode is added by the MCP server
		if err != nil || !want.Get("structuredContent.auth_mode").Exists() {
			t.Fatalf("%s: golden sample has no auth_mode (%v)", c.file, err)
		}
		f.up.always(c.route, 200, api)
		tool := strings.TrimSuffix(strings.SplitN(c.file, "-", 3)[2], ".json")
		got := f.call("/tavily/mcp", tool, args.Raw, nil)
		if text(got) != text(want) {
			t.Errorf("%s text differs from the official server:\n got %.300s\nwant %.300s", tool, text(got), text(want))
		}
		if got.Get("structuredContent").Raw != text(want) || got.Get("isError").Raw != "false" {
			t.Errorf("%s: structuredContent/isError = %.200s %s", tool, got.Get("structuredContent").Raw, got.Get("isError").Raw)
		}
	}
}

func TestTavilyRequestBody(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.up.always("POST /search", 200, `{"query":"q","results":[],"usage":{"credits":1}}`)
	// Checked against the official server (2026-11-10): what it sends is visible in the API's 422 answers.
	got := f.call("/tavily/mcp", "tavily_search",
		`{"query":"q","max_results":"3","country":"germany","topic":"news","start_date":"2026-01-01","time_range":"week","include_domains":[],"exclude_domains":null,"include_favicon":"yes","end_date":""}`,
		map[string]string{"DEFAULT_PARAMETERS": `{"max_results":10,"include_images":true,"not_a_tool_param":1}`})
	body := gjson.Parse(f.up.last(t, "POST /search").body)
	checks := map[string]string{
		"query": `"q"`, "max_results": "3", // an explicit argument beats DEFAULT_PARAMETERS; "3" arrives as a number
		"include_images":  "true",   // a default fills what the call left out
		"include_favicon": "true",   // lax booleans arrive as booleans
		"topic":           `"news"`, // a country does not change the topic
		"country":         `"germany"`,
		"include_usage":   "true", // precise billing
	}
	for k, want := range checks {
		if v := body.Get(k).Raw; v != want {
			t.Errorf("upstream %s = %s, want %s (body %s)", k, v, want, body.Raw)
		}
	}
	for _, k := range []string{"time_range", "include_domains", "exclude_domains", "end_date", "not_a_tool_param", "api_key"} {
		if body.Get(k).Exists() {
			t.Errorf("upstream body has %s: %s", k, body.Raw)
		}
	}
	if strings.Contains(text(got), "usage") {
		t.Errorf("the usage the gateway asked for leaked to the client: %s", text(got))
	}
	if l := f.rec.all(); len(l) != 1 || l[0].Surface != "mcp" || l[0].Operation != "search" || l[0].QueryPreview != "q" || l[0].CostCredits != 1 {
		t.Errorf("request log = %+v", l)
	}
}

func TestTavilyErrorsMatchOfficial(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	args, want := golden(t, "tavily-04-tavily_search.json")
	f.up.always("POST /search", 422, `{"detail":`+want.Get("structuredContent.detail").Raw+`}`)
	got := f.call("/tavily/mcp", "tavily_search", args.Raw, nil)
	if text(got) != text(want) || got.Get("isError").Raw != "false" {
		t.Fatalf("422:\n got %s\nwant %s", got.Raw, want.Raw)
	}
	// Errors produced by the gateway itself are flagged as errors.
	g := newFixture(t, fixtureOpts{tokens: "tokens:\n  - name: paid\n    token: " + tok + "\n    key_group: paid\n"})
	got = g.call("/tavily/mcp", "tavily_search", `{"query":"q"}`, nil)
	if !got.Get("isError").Bool() || !strings.Contains(text(got), "no available tavily key") {
		t.Fatalf("gateway error = %s", got.Raw)
	}
}

func TestTavilyResearchPollsTheCreatingKey(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.up.always("POST /research", 200, `{"request_id":"r-9","status":"pending"}`)
	f.up.on("GET /research/r-9", func(n int, _ string) (int, string) {
		if n < 3 {
			return 200, `{"request_id":"r-9","status":"in_progress"}`
		}
		return 200, `{"request_id":"r-9","status":"completed","content":"# Report"}`
	})
	start := f.now()
	got := f.call("/tavily/mcp", "tavily_research", `{"input":"what changed in go 1.27","model":"mini"}`, nil)
	if text(got) != `{"request_id":"r-9","status":"completed","content":"# Report","auth_mode":"keyed"}` || got.Get("isError").Bool() {
		t.Fatalf("result = %s", got.Raw)
	}
	if b := gjson.Parse(f.up.last(t, "POST /research").body); b.Get("input").String() == "" || b.Get("model").String() != "mini" {
		t.Fatalf("create body = %s", b.Raw)
	}
	// Polls back off 2 s, 3 s, 4.5 s, 6.75 s and all use the key that created the task.
	if waited := f.now().Sub(start).Seconds(); waited != 2+3+4.5+6.75 {
		t.Errorf("waited %.2fs", waited)
	}
	createKey := f.up.last(t, "POST /research").header.Get("Authorization")
	f.up.mu.Lock()
	for _, r := range f.up.reqs {
		if r.path == "/research/r-9" && r.header.Get("Authorization") != createKey {
			t.Errorf("a poll used another key")
		}
	}
	f.up.mu.Unlock()
	// Only the create call is logged; the polls are quiet.
	if l := f.rec.all(); len(l) != 1 || l[0].Operation != "research" {
		t.Errorf("logs = %+v", l)
	}

	// A task that never ends gives up after the model's limit (mini: 5 minutes).
	f.up.always("POST /research", 200, `{"request_id":"r-10"}`)
	f.up.always("GET /research/r-10", 200, `{"request_id":"r-10","status":"in_progress"}`)
	start = f.now()
	got = f.call("/tavily/mcp", "tavily_research", `{"input":"x","model":"mini"}`, nil)
	if !got.Get("isError").Bool() || !strings.Contains(text(got), "timed out") || f.now().Sub(start) > 5*60e9 {
		t.Fatalf("timeout = %s after %v", got.Raw, f.now().Sub(start))
	}
}

// ---- Exa ------------------------------------------------------------------------

// exaResultsFromText rebuilds the API answer behind a golden web_search_exa text.
func exaResultsFromText(t *testing.T, s string) []map[string]any {
	var out []map[string]any
	for _, part := range strings.Split(s, "\n\n---\n\n") {
		r := map[string]any{}
		head, hl, _ := strings.Cut(part, "\nHighlights:\n")
		for _, line := range strings.Split(head, "\n") {
			k, v, _ := strings.Cut(line, ": ")
			if v == "N/A" {
				continue
			}
			switch k {
			case "Title":
				r["title"] = v
			case "URL":
				r["url"] = v
			case "Published":
				r["publishedDate"] = v
			case "Author":
				r["author"] = v
			}
		}
		r["highlights"] = []string{hl}
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatal("no results in the golden text")
	}
	return out
}

func TestExaWebSearchMatchesOfficial(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	args, want := golden(t, "exa-05-web_search_exa.json")
	api := map[string]any{"requestId": "x", "results": exaResultsFromText(t, text(want)),
		"searchTime": json.RawMessage(want.Get("content.0._meta.searchTime").Raw), "costDollars": map[string]any{"total": 0.007}}
	b, _ := json.Marshal(api)
	f.up.always("POST /search", 200, string(b))
	got := f.call("/exa/mcp", "web_search_exa", args.Raw, nil)
	if text(got) != text(want) {
		t.Fatalf("text differs:\n got %.400s\nwant %.400s", text(got), text(want))
	}
	if got.Get(`_meta.ai\.exa/usage.costDollars`).Raw != "0.007" || got.Get("content.0._meta.searchTime").Raw != want.Get("content.0._meta.searchTime").Raw || got.Get("isError").Exists() {
		t.Fatalf("meta = %s", got.Raw)
	}
	body := gjson.Parse(f.up.last(t, "POST /search").body)
	if body.Get("type").String() != "auto" || body.Get("numResults").Int() != 2 || body.Get("contents.highlights").Raw != "true" ||
		body.Get("objective").String() != args.Get("objective").String() {
		t.Fatalf("request = %s", body.Raw)
	}
}

func TestExaWebSearchOptions(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.up.always("POST /search", 200, `{"results":[],"notices":[{"code":"relaxed","path":"query","message":"Filters were relaxed."},{"code":"bad"}],"warnings":[{"code":"w","path":"p","message":"internal"}]}`)
	got := f.call("/exa/mcp?dynamicHighlights=true&defaultSearchType=fast", "web_search_exa", `{"query":"category:People  Jane Doe engineer","numResults":"5"}`, nil)
	if text(got) != "Note: Filters were relaxed.\n\nNo search results found. Please try a different query." {
		t.Fatalf("text = %q", text(got))
	}
	s := f.up.last(t, "POST /search")
	body := gjson.Parse(s.body)
	if body.Get("query").String() != "Jane Doe engineer" || body.Get("category").String() != "people" || body.Get("type").String() != "fast" ||
		body.Get("numResults").Int() != 5 || body.Get("contents.highlights.dynamic").Raw != "true" || s.header.Get("Exa-Beta") != exaDynamicHighlightsBeta {
		t.Fatalf("request = %s %v", body.Raw, s.header)
	}
	if got := f.call("/exa/mcp", "web_search_exa", `{"query":"  "}`, nil); !got.Get("isError").Bool() {
		t.Fatalf("empty query accepted: %s", got.Raw)
	}
	// Upstream errors use the official wording.
	f.up.always("POST /search", 400, `{"error":"numResults must be at most 100","tag":"INVALID_REQUEST"}`)
	if got := f.call("/exa/mcp", "web_search_exa", `{"query":"x","numResults":500}`, nil); text(got) != "web_search_exa error (400): numResults must be at most 100" || !got.Get("isError").Bool() {
		t.Fatalf("error = %s", got.Raw)
	}
}

func TestExaWebFetchMatchesOfficial(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	args, want := golden(t, "exa-06-web_fetch_exa.json")
	head, body, _ := strings.Cut(text(want), "\n\n")
	title := strings.TrimPrefix(strings.SplitN(head, "\n", 2)[0], "# ")
	u := strings.TrimPrefix(strings.SplitN(head, "\n", 2)[1], "URL: ")
	api, _ := json.Marshal(map[string]any{"results": []map[string]any{{"title": title, "url": u, "text": body}},
		"statuses": []map[string]any{{"id": u, "status": "success"}}, "costDollars": map[string]any{"total": 0.001},
		"searchTime": json.RawMessage(want.Get("content.0._meta.searchTime").Raw)})
	f.up.always("POST /contents", 200, string(api))
	got := f.call("/exa/mcp", "web_fetch_exa", args.Raw, nil)
	if text(got) != text(want) || got.Get(`_meta.ai\.exa/usage.costDollars`).Raw != "0.001" {
		t.Fatalf("fetch:\n got %.300s\nwant %.300s", got.Raw, text(want))
	}
	if b := gjson.Parse(f.up.last(t, "POST /contents").body); b.Get("text.maxCharacters").Int() != 400 || b.Get("urls.0").String() != u {
		t.Fatalf("request = %s", b.Raw)
	}

	args, want = golden(t, "exa-08-web_fetch_exa.json")
	bad := args.Get("urls.0").String()
	f.up.always("POST /contents", 200, `{"results":[],"statuses":[{"id":"`+bad+`","status":"error","error":{"tag":"CRAWL_UNKNOWN_ERROR","httpStatusCode":null}}]}`)
	got = f.call("/exa/mcp", "web_fetch_exa", args.Raw, nil)
	if text(got) != text(want) || !got.Get("isError").Bool() {
		t.Fatalf("failed fetch:\n got %s\nwant %s", got.Raw, want.Raw)
	}
	// A single string is accepted for urls, and the default length applies.
	f.up.always("POST /contents", 200, `{"results":[{"title":"T","url":"https://a","publishedDate":"2026-01-02T03:04:05Z","author":"A","text":"body"}]}`)
	got = f.call("/exa/mcp", "web_fetch_exa", `{"urls":"https://a"}`, nil)
	if text(got) != "# T\nURL: https://a\nPublished: 2026-01-02\nAuthor: A\n\nbody" {
		t.Fatalf("text = %q", text(got))
	}
	if b := gjson.Parse(f.up.last(t, "POST /contents").body); b.Get("text.maxCharacters").Int() != 3000 || b.Get("urls.#").Int() != 1 {
		t.Fatalf("request = %s", b.Raw)
	}
}

func TestExaAdvancedSearchMatchesOfficial(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	args, want := golden(t, "exa-07-web_search_advanced_exa.json")
	// The API's answer carries more than the official server shows a model.
	api := gjson.Parse(text(want))
	raw := api.Raw
	raw = strings.Replace(raw, `"results":[{`, `"requestTags":{"t":1},"debug":{"x":1},"results":[{"unknownField":1,`, 1)
	raw = strings.Replace(raw, `"costDollars":{`, `"costDollars":{"requestTags":["x"],`, 1)
	f.up.always("POST /search", 200, raw)
	got := f.call("/exa/mcp?tools=web_search_advanced_exa", "web_search_advanced_exa", args.Raw, nil)
	if text(got) != text(want) {
		t.Fatalf("text differs:\n got %.400s\nwant %.400s", text(got), text(want))
	}
	body := gjson.Parse(f.up.last(t, "POST /search").body)
	if body.Get("includeDomains.0").String() != "go.dev" || body.Get("contents.livecrawl").String() != "fallback" ||
		body.Get("contents.text").Exists() || body.Get("contents.highlights").Raw != "{}" || body.Get("numResults").Raw != "2" {
		t.Fatalf("request = %s", body.Raw)
	}

	// lite refuses what it cannot serve, before any upstream call.
	got = f.call("/exa/mcp?tools=web_search_advanced_exa", "web_search_advanced_exa", `{"query":"q","type":"lite","includeText":["a"],"enableSummary":true}`, nil)
	if text(got) != "web_search_advanced_exa error: search type 'lite' does not support includeText, enableSummary. Remove them, or set type to 'auto'." {
		t.Fatalf("lite = %q", text(got))
	}
	got = f.call("/exa/mcp?tools=web_search_advanced_exa&dynamicHighlights=true", "web_search_advanced_exa", `{"query":"q","enableHighlights":true,"highlightsMaxCharacters":100}`, nil)
	if !strings.Contains(text(got), "highlightsMaxCharacters is not supported") {
		t.Fatalf("dynamic cap = %q", text(got))
	}
}

func TestExaAgentRun(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.up.always("POST /agent/runs", 200, `{"id":"agent_run_1","status":"queued"}`)
	f.up.on("GET /agent/runs/agent_run_1", func(n int, _ string) (int, string) {
		if n < 2 {
			return 200, `{"id":"agent_run_1","status":"running"}`
		}
		return 200, `{"id":"agent_run_1","status":"completed","output":{"text":"done"},"stopReason":"complete","usage":{"searches":3},"costDollars":{"total":0.25}}`
	})
	got := f.call("/exa/mcp", "agent_run", `{"query":"list Go releases","dataSources":[{"provider":"fiber","extra":1}]}`, nil)
	want := `{"success":true,"id":"agent_run_1","status":"completed","outputReady":true,"output":{"text":"done"},"stopReason":"complete","usage":{"searches":3},"costDollars":{"total":0.25}}`
	if text(got) != want || got.Get("structuredContent").Raw != want || got.Get("isError").Exists() {
		t.Fatalf("result = %s", got.Raw)
	}
	create := gjson.Parse(f.up.last(t, "POST /agent/runs").body)
	if create.Get("effort").String() != "low" || create.Get("dataSources").Raw != `[{"provider":"fiber"}]` {
		t.Fatalf("create = %s", create.Raw)
	}
	// Billed once, when the run ended, on the key that created it.
	used := func() int64 {
		var sum int64
		for _, v := range f.pool.Views() {
			if v.Provider == "exa" && v.Used != nil {
				sum += *v.Used
			}
		}
		return sum
	}
	if used() != 250_000 {
		t.Fatalf("charged %d micro-USD", used())
	}
	again := f.call("/exa/mcp", "agent_run", `{"runId":"agent_run_1"}`, nil)
	if text(again) != want || used() != 250_000 {
		t.Fatalf("resume of a finished run: %s, charged %d", again.Raw, used())
	}

	// A run that outlasts the call window is handed back with its id.
	f.up.always("POST /agent/runs", 200, `{"id":"agent_run_2","status":"queued"}`)
	f.up.always("GET /agent/runs/agent_run_2", 200, `{"id":"agent_run_2","status":"running"}`)
	start := f.now()
	got = f.call("/exa/mcp", "agent_run", `{"query":"slow"}`, nil)
	if got.Get("structuredContent.status").String() != "running" || !strings.Contains(got.Get("structuredContent.message").String(), "runId=agent_run_2") {
		t.Fatalf("running = %s", got.Raw)
	}
	if d := f.now().Sub(start); d < f.srv.o.AgentCallWindow || d > f.srv.o.AgentCallWindow+f.srv.o.AgentPollInterval {
		t.Fatalf("waited %v", d)
	}

	// A failed run is an error, with the official follow-up hint.
	f.up.always("GET /agent/runs/agent_run_3", 200, `{"id":"agent_run_3","status":"failed","error":{"message":"bad schema"}}`)
	got = f.call("/exa/mcp", "agent_run", `{"runId":"agent_run_3"}`, nil)
	if !got.Get("isError").Bool() || got.Get("structuredContent.error.message").String() != "bad schema" || got.Get("content.#").Int() != 2 {
		t.Fatalf("failed = %s", got.Raw)
	}

	for in, msg := range map[string]string{
		`{}`:                                  "Provide exactly one of query or runId",
		`{"query":"a","runId":"agent_run_1"}`: "Provide exactly one of query or runId",
		`{"runId":"agent_run_1","effort":"high"}`: "accepts only runId",
	} {
		if got := f.call("/exa/mcp", "agent_run", in, nil); !got.Get("isError").Bool() || !strings.Contains(text(got), msg) {
			t.Errorf("%s: %s", in, got.Raw)
		}
	}
	f.up.always("POST /agent/runs", 402, `{"error":"You have run out of credits","tag":"NO_MORE_CREDITS"}`)
	got = f.call("/exa/mcp", "agent_run", `{"query":"x"}`, nil)
	if !strings.HasPrefix(text(got), "agent_run error (402): You have run out of credits\n\nNot retryable") {
		t.Fatalf("402 = %s", got.Raw)
	}
}

func TestSnapshotsHaveNoSecrets(t *testing.T) {
	// The snapshots are fetched with a real key; it must never be stored.
	for _, p := range confstore.Providers {
		b, _ := schemaFS.ReadFile("schemas/" + p + ".json")
		for _, s := range []string{"tvly-", "tavilyApiKey=", "exaApiKey="} {
			if strings.Contains(string(b), s) {
				t.Errorf("%s snapshot contains %q", p, s)
			}
		}
	}
}
