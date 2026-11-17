package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/httpx"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
	"github.com/sodiseng/search-warden/gateway/internal/relay"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

const tok = "test-mcp-token-0123456789"

type seen struct {
	method, path string
	header       http.Header
	body         string
}

// fakeUpstream answers Tavily and Exa REST calls from a per-path script.
type fakeUpstream struct {
	mu     sync.Mutex
	reqs   []seen
	routes map[string]func(n int, body string) (int, string) // "METHOD /path" → answer; n counts calls to the route
	counts map[string]int
	srv    *httptest.Server
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	u := &fakeUpstream{routes: map[string]func(int, string) (int, string){}, counts: map[string]int{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		route := r.Method + " " + r.URL.Path
		u.mu.Lock()
		u.reqs = append(u.reqs, seen{r.Method, r.URL.Path, r.Header.Clone(), string(b)})
		n := u.counts[route]
		u.counts[route]++
		h := u.routes[route]
		u.mu.Unlock()
		if h == nil {
			http.Error(w, `{"error":"no route `+route+`"}`, http.StatusNotFound)
			return
		}
		status, body := h(n, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *fakeUpstream) on(route string, h func(n int, body string) (int, string)) {
	u.mu.Lock()
	u.routes[route] = h
	u.mu.Unlock()
}
func (u *fakeUpstream) always(route string, status int, body string) {
	u.on(route, func(int, string) (int, string) { return status, body })
}

func (u *fakeUpstream) last(t *testing.T, route string) seen {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.reqs) - 1; i >= 0; i-- {
		if u.reqs[i].method+" "+u.reqs[i].path == route {
			return u.reqs[i]
		}
	}
	t.Fatalf("upstream never saw %s", route)
	return seen{}
}

type logRec struct {
	mu   sync.Mutex
	logs []store.RequestLog
}

func (r *logRec) Record(l store.RequestLog, _ []store.Attempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, l)
}

func (r *logRec) all() []store.RequestLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.RequestLog(nil), r.logs...)
}

type fixture struct {
	t     *testing.T
	up    *fakeUpstream
	pool  *pool.Pool
	rec   *logRec
	srv   *Server
	gw    *httptest.Server
	mu    sync.Mutex
	clock time.Time
}

type fixtureOpts struct {
	tokens string // tokens.yaml; default: one token for everything
	keys   map[string]string
}

func newFixture(t *testing.T, fo fixtureOpts) *fixture {
	t.Helper()
	dir := t.TempDir()
	w := func(name, content string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if fo.tokens == "" {
		fo.tokens = "tokens:\n  - name: all\n    token: " + tok + "\n"
	}
	keys := map[string]string{"tavily": "  - tvly-key-a\n  - tvly-key-b\n", "exa": "  - exa-key-a\n  - exa-key-b\n"}
	for k, v := range fo.keys {
		keys[k] = v
	}
	w("keys/tavily.yaml", "keys:\n"+keys["tavily"])
	w("keys/exa.yaml", "keys:\n"+keys["exa"])
	w("tokens.yaml", fo.tokens)
	conf, err := confstore.Open(dir, confstore.Options{AdminPasswordEnv: "correct-horse-battery"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conf.SetConfig(nil, "routing.strategy", "drain-first"); err != nil {
		t.Fatal(err)
	}
	up := newFakeUpstream(t)
	out := outbound.New(outbound.Options{})
	p := pool.New(pool.Options{EgressUsable: out.Usable})
	out.Apply(conf.Snapshot())
	p.Apply(conf.Snapshot())
	rec := &logRec{}
	rl := relay.New(relay.Options{Conf: conf, Pool: p, Out: out, Rec: rec,
		Providers: map[string]provider.Provider{"tavily": provider.NewTavily(up.srv.URL), "exa": provider.NewExa(up.srv.URL)}})
	f := &fixture{t: t, up: up, pool: p, rec: rec, clock: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	srv, err := New(Options{Conf: conf, Relay: rl, Now: f.now, KeepAlive: 20 * time.Millisecond,
		Sleep: func(ctx context.Context, d time.Duration) bool { // instant, but moves the fake clock
			if ctx.Err() != nil {
				return false
			}
			f.mu.Lock()
			f.clock = f.clock.Add(d)
			f.mu.Unlock()
			return true
		}})
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	res, _ := httpx.NewClientIPResolver(httpx.DefaultTrustedProxies(), httpx.DefaultClientIPHeaders())
	mux := http.NewServeMux()
	for path, scope := range map[string]string{"/tavily/mcp": "tavily", "/exa/mcp": "exa", "/mcp": ""} {
		mux.Handle(path, srv.Handler(scope))
		mux.Handle(path+"/", srv.Handler(scope))
	}
	f.gw = httptest.NewServer(httpx.Chain(mux, httpx.RequestID(), httpx.ResolveClientIP(func() *httpx.ClientIPResolver { return res })))
	t.Cleanup(f.gw.Close)
	return f
}

func (f *fixture) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clock
}

type reply struct {
	status int
	header http.Header
	raw    string
	msgs   []gjson.Result // JSON body, or every SSE event in order
}

// final is the JSON-RPC response (the last message).
func (r reply) final() gjson.Result {
	if len(r.msgs) == 0 {
		return gjson.Result{}
	}
	return r.msgs[len(r.msgs)-1]
}

func (f *fixture) post(path, body string, hdr map[string]string) reply {
	f.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.gw.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	r := reply{status: resp.StatusCode, header: resp.Header}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		var raw strings.Builder
		for sc.Scan() {
			line := sc.Text()
			raw.WriteString(line + "\n")
			if d, ok := strings.CutPrefix(line, "data: "); ok {
				r.msgs = append(r.msgs, gjson.Parse(d))
			}
		}
		r.raw = raw.String()
		return r
	}
	b, _ := io.ReadAll(resp.Body)
	r.raw = string(b)
	if gjson.ValidBytes(b) {
		r.msgs = []gjson.Result{gjson.ParseBytes(b)}
	}
	return r
}

func rpc(id int, method, params string) string {
	if params == "" {
		params = "{}"
	}
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `","params":` + params + `}`
}

func itoa(n int) string { return strings.TrimSpace(string(marshal(n))) }

// modern returns the request metadata a 2026-07-28 client sends.
func modernMeta(extra string) string {
	m := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}` + extra + `}`
	return m
}

func modernHeaders(method, name string) map[string]string {
	h := map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": method}
	if name != "" {
		h["Mcp-Name"] = name
	}
	return h
}

// call runs tools/call over a legacy (2025-11-25) connection and returns the result object.
func (f *fixture) call(path, tool, args string, hdr map[string]string) gjson.Result {
	f.t.Helper()
	h := map[string]string{"MCP-Protocol-Version": "2025-11-25"}
	for k, v := range hdr {
		h[k] = v
	}
	r := f.post(path, rpc(9, "tools/call", `{"name":"`+tool+`","arguments":`+args+`}`), h)
	if r.status != http.StatusOK || !r.final().Get("result").Exists() {
		f.t.Fatalf("tools/call %s: %d %s", tool, r.status, r.raw)
	}
	return r.final().Get("result")
}

func snapshotTools(t *testing.T, provider string) []gjson.Result {
	b, err := schemaFS.ReadFile("schemas/" + provider + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return gjson.GetBytes(b, "tools").Array()
}

func toolNames(r gjson.Result) []string {
	var out []string
	for _, t := range r.Get("result.tools").Array() {
		out = append(out, t.Get("name").String())
	}
	return out
}

// ---- protocol -------------------------------------------------------------------

func TestLegacyHandshakeAndToolList(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	r := f.post("/tavily/mcp", rpc(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`), map[string]string{"MCP-Protocol-Version": ""})
	res := r.final().Get("result")
	if r.status != 200 || res.Get("protocolVersion").String() != "2025-06-18" || res.Get("serverInfo.name").String() != "tavily-mcp" ||
		!strings.HasPrefix(res.Get("instructions").String(), "Researching with Tavily") || res.Get("capabilities.tools").Raw == "" {
		t.Fatalf("initialize = %d %s", r.status, r.raw)
	}
	if r.header.Get("Mcp-Session-Id") != "" {
		t.Fatal("the gateway must not issue sessions")
	}
	// An unknown requested version gets the newest legacy version.
	if v := f.post("/exa/mcp", rpc(1, "initialize", `{"protocolVersion":"2024-11-05"}`), nil).final().Get("result.protocolVersion").String(); v != "2025-11-25" {
		t.Fatalf("negotiated %q", v)
	}
	if r := f.post("/tavily/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, map[string]string{"MCP-Protocol-Version": "2025-06-18"}); r.status != http.StatusAccepted || r.raw != "" {
		t.Fatalf("notification = %d %q", r.status, r.raw)
	}

	// tools/list is the official snapshot, byte for byte.
	list := f.post("/tavily/mcp", rpc(2, "tools/list", ""), map[string]string{"MCP-Protocol-Version": "2025-06-18"}).final()
	want := snapshotTools(t, "tavily")
	got := list.Get("result.tools").Array()
	if len(got) != len(want) || len(got) != 5 {
		t.Fatalf("tavily tools = %v", toolNames(list))
	}
	for i := range want {
		var w bytes.Buffer
		_ = json.Compact(&w, []byte(want[i].Raw))
		if got[i].Raw != w.String() {
			t.Fatalf("tool %d differs from the official snapshot:\n%s\n%s", i, got[i].Raw, w.String())
		}
	}
	if list.Get("result.ttlMs").Exists() || list.Get("result.resultType").Exists() {
		t.Fatal("legacy results must not carry modern fields")
	}
}

func TestToolSelection(t *testing.T) {
	f := newFixture(t, fixtureOpts{tokens: "tokens:\n  - name: all\n    token: " + tok + "\n" +
		"  - name: tavily-only\n    token: tavily-only-token-0123456789\n    providers: [tavily]\n" +
		"  - name: two-tools\n    token: two-tools-token-0123456789\n    tools: [tavily_search, web_search_exa]\n"})
	list := func(path string, token string) []string {
		h := map[string]string{}
		if token != "" {
			h["Authorization"] = "Bearer " + token
		}
		return toolNames(f.post(path, rpc(1, "tools/list", ""), h).final())
	}
	cases := []struct {
		path, token, want string
	}{
		{"/exa/mcp", "", "web_search_exa,web_fetch_exa,agent_run"}, // official default set
		{"/exa/mcp?tools=web_search_advanced_exa,web_search_exa", "", "web_search_exa,web_search_advanced_exa"},
		{"/exa/mcp?tools=agent_tools", "", "agent_run"},
		{"/exa/mcp?tools=nonsense", "", "web_search_exa,web_fetch_exa,agent_run"},
		{"/mcp", "", "tavily_search,tavily_extract,tavily_crawl,tavily_map,tavily_research,web_search_exa,web_fetch_exa,agent_run"},
		{"/mcp?providers=exa", "", "web_search_exa,web_fetch_exa,agent_run"},
		{"/mcp?tools=tavily_search,web_fetch_exa", "", "tavily_search,web_fetch_exa"},
		{"/mcp?exaApiKey=" + tok, "-", "web_search_exa,web_fetch_exa,agent_run"}, // only the host replaced
		{"/mcp", "tavily-only-token-0123456789", "tavily_search,tavily_extract,tavily_crawl,tavily_map,tavily_research"},
		{"/mcp", "two-tools-token-0123456789", "tavily_search,web_search_exa"},
	}
	for _, c := range cases {
		token := c.token
		if token == "-" {
			token = "" // the token travels in the URL
		}
		got := ""
		if c.token == "-" {
			r := f.post(c.path, rpc(1, "tools/list", ""), map[string]string{"Authorization": ""})
			got = strings.Join(toolNames(r.final()), ",")
		} else {
			got = strings.Join(list(c.path, token), ",")
		}
		if got != c.want {
			t.Errorf("%s (%s): %s, want %s", c.path, c.token, got, c.want)
		}
	}
	if r := f.post("/exa/mcp", rpc(1, "tools/list", ""), map[string]string{"Authorization": "Bearer tavily-only-token-0123456789"}); r.status != http.StatusForbidden {
		t.Fatalf("tavily-only token on /exa/mcp = %d", r.status)
	}
	// A tool outside the token's scope cannot be called either.
	r := f.post("/mcp", rpc(1, "tools/call", `{"name":"tavily_map","arguments":{"url":"https://x"}}`), map[string]string{"Authorization": "Bearer two-tools-token-0123456789"})
	if e := r.final().Get("error"); e.Get("code").Int() != codeInvalidParams || !strings.Contains(e.Get("message").String(), "Unknown tool") {
		t.Fatalf("out-of-scope call = %s", r.raw)
	}
}

func TestModernProtocol(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	r := f.post("/tavily/mcp", rpc(1, "server/discover", "{"+modernMeta("")+"}"), modernHeaders("server/discover", ""))
	res := r.final().Get("result")
	if r.status != 200 || res.Get("resultType").String() != "complete" || res.Get("supportedVersions.0").String() != "2026-07-28" ||
		res.Get(`_meta.io\.modelcontextprotocol/serverInfo.name`).String() != "tavily-mcp" || res.Get("ttlMs").Int() <= 0 ||
		res.Get("cacheScope").String() != "private" || res.Get("instructions").String() == "" {
		t.Fatalf("discover = %d %s", r.status, r.raw)
	}
	list := f.post("/exa/mcp", rpc(2, "tools/list", "{"+modernMeta("")+"}"), modernHeaders("tools/list", "")).final().Get("result")
	if len(list.Get("tools").Array()) != 3 || list.Get("resultType").String() != "complete" || list.Get("cacheScope").String() != "private" {
		t.Fatalf("modern tools/list = %s", list.Raw)
	}

	errCase := func(name string, body string, hdr map[string]string, status int, code int64) {
		t.Helper()
		r := f.post("/tavily/mcp", body, hdr)
		if r.status != status || r.final().Get("error.code").Int() != code {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
	errCase("missing Mcp-Method", rpc(3, "tools/list", "{"+modernMeta("")+"}"), map[string]string{"MCP-Protocol-Version": "2026-07-28"}, 400, codeHeaderMismatch)
	errCase("header/body version mismatch", rpc(3, "tools/list", "{"+modernMeta("")+"}"), map[string]string{"MCP-Protocol-Version": "2025-11-25", "Mcp-Method": "tools/list"}, 400, codeHeaderMismatch)
	errCase("modern header without _meta", rpc(3, "tools/list", ""), modernHeaders("tools/list", ""), 400, codeHeaderMismatch)
	errCase("Mcp-Name mismatch", rpc(3, "tools/call", `{"name":"tavily_search","arguments":{},`+modernMeta("")+`}`), modernHeaders("tools/call", "tavily_map"), 400, codeHeaderMismatch)
	errCase("unknown method", rpc(3, "resources/subscribe", "{"+modernMeta("")+"}"), modernHeaders("resources/subscribe", ""), 404, codeMethodNotFound)
	errCase("ping is gone in 2026-07-28", rpc(3, "ping", "{"+modernMeta("")+"}"), modernHeaders("ping", ""), 404, codeMethodNotFound)

	r = f.post("/tavily/mcp", rpc(4, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2099-01-01"}}`), map[string]string{"MCP-Protocol-Version": "2099-01-01", "Mcp-Method": "tools/list"})
	e := r.final().Get("error")
	if r.status != 400 || e.Get("code").Int() != codeUnsupportedVersion || e.Get("data.requested").String() != "2099-01-01" || !strings.Contains(e.Get("data.supported").Raw, "2025-11-25") {
		t.Fatalf("unsupported version = %d %s", r.status, r.raw)
	}

	// A tool name sent with the Base64 sentinel encoding.
	f.up.always("POST /search", 200, `{"query":"q","results":[]}`)
	name := "=?base64?" + base64.StdEncoding.EncodeToString([]byte("tavily_search")) + "?="
	r = f.post("/tavily/mcp", rpc(5, "tools/call", `{"name":"tavily_search","arguments":{"query":"q"},`+modernMeta("")+`}`), modernHeaders("tools/call", name))
	if res := r.final().Get("result"); r.status != 200 || res.Get("resultType").String() != "complete" || res.Get("isError").Bool() {
		t.Fatalf("modern tools/call = %d %s", r.status, r.raw)
	}
}

func TestTransportRules(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequest(m, f.gw.URL+"/tavily/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodPost {
			t.Errorf("%s = %d", m, resp.StatusCode)
		}
	}
	list := rpc(1, "tools/list", "")
	if r := f.post("/tavily/mcp", list, map[string]string{"Origin": "https://evil.example"}); r.status != http.StatusForbidden {
		t.Errorf("foreign Origin = %d", r.status)
	}
	if r := f.post("/tavily/mcp", list, map[string]string{"Origin": f.gw.URL}); r.status != 200 {
		t.Errorf("same-host Origin = %d", r.status)
	}
	if r := f.post("/tavily/mcp?exaApiKey="+tok, list, map[string]string{"Authorization": ""}); r.status != 400 {
		t.Errorf("Exa key parameter on the Tavily endpoint = %d", r.status)
	}
	r := f.post("/tavily/mcp", list, map[string]string{"Authorization": "Bearer wrong-token-0123456789"})
	if r.status != 401 || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Bearer") {
		t.Errorf("bad token = %d %v", r.status, r.header)
	}
	if r := f.post("/tavily/mcp?tavilyApiKey="+tok, list, map[string]string{"Authorization": ""}); r.status != 200 {
		t.Errorf("official-style URL key = %d %s", r.status, r.raw)
	}
	if r := f.post("/tavily/mcp", "{not json", nil); r.status != 400 || r.final().Get("error.code").Int() != codeParseError {
		t.Errorf("parse error = %d %s", r.status, r.raw)
	}
	// 2025-03-26 batch: answers for requests only, in order.
	r = f.post("/tavily/mcp", "["+rpc(1, "ping", "")+`,{"jsonrpc":"2.0","method":"notifications/initialized"},`+rpc(2, "tools/list", "")+"]", map[string]string{"MCP-Protocol-Version": "2025-03-26"})
	arr := r.final().Array()
	if r.status != 200 || len(arr) != 2 || arr[0].Get("id").Int() != 1 || len(arr[1].Get("result.tools").Array()) != 5 {
		t.Fatalf("batch = %d %s", r.status, r.raw)
	}
	// Optional lists answer empty, like the official Tavily server.
	if r := f.post("/tavily/mcp", rpc(3, "prompts/list", ""), nil); r.final().Get("result.prompts").Raw != "[]" {
		t.Errorf("prompts/list = %s", r.raw)
	}
}

func TestSSEKeepAliveAndProgress(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.up.always("POST /research", 200, `{"request_id":"r-1","status":"pending"}`)
	f.up.on("GET /research/r-1", func(n int, _ string) (int, string) {
		time.Sleep(30 * time.Millisecond) // long enough for a keep-alive comment
		if n < 2 {
			return 200, `{"request_id":"r-1","status":"in_progress"}`
		}
		return 200, `{"request_id":"r-1","status":"completed","content":"report"}`
	})
	r := f.post("/tavily/mcp", rpc(7, "tools/call", `{"name":"tavily_research","arguments":{"input":"x"},"_meta":{"progressToken":"p-1"}}`), map[string]string{"MCP-Protocol-Version": "2025-11-25"})
	if r.status != 200 || r.header.Get("X-Accel-Buffering") != "no" || !strings.Contains(r.raw, ": keepalive") {
		t.Fatalf("stream = %d %v\n%s", r.status, r.header, r.raw)
	}
	var progress []string
	for _, m := range r.msgs[:len(r.msgs)-1] {
		if m.Get("method").String() != "notifications/progress" || m.Get("params.progressToken").String() != "p-1" {
			t.Fatalf("unexpected message before the response: %s", m.Raw)
		}
		progress = append(progress, m.Get("params.message").String())
	}
	if len(progress) < 3 || r.final().Get("id").Int() != 7 || r.final().Get("result.structuredContent.content").String() != "report" {
		t.Fatalf("progress %v, final %s", progress, r.final().Raw)
	}
	// Without Accept: text/event-stream the answer is plain JSON.
	r = f.post("/tavily/mcp", rpc(8, "tools/call", `{"name":"tavily_research","arguments":{"input":"x"}}`), map[string]string{"Accept": "application/json"})
	if !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") || r.final().Get("result.content.0.text").String() == "" {
		t.Fatalf("json mode = %v %s", r.header, r.raw)
	}
}

func TestMarshalLikeJavaScript(t *testing.T) {
	o := newOrdered().set("b", "<a & b>").set("a", json.RawMessage(`1`)).set("c", nil)
	if s := string(marshal(o)); s != `{"b":"<a & b>","a":1,"c":null}` {
		t.Fatal(s)
	}
	o.del("a")
	if s := string(marshal(o)); s != `{"b":"<a & b>","c":null}` {
		t.Fatal(s)
	}
}
