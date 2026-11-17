package relay

import (
	"bufio"
	"context"
	"fmt"
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
	"github.com/sodiseng/search-warden/gateway/internal/store"
	"github.com/sodiseng/search-warden/gateway/internal/testutil/socks5"
)

const clientToken = "test-client-token-0123456789"

type seenReq struct {
	method, path, query string
	header              http.Header
	body                string
}

// upstream is a scriptable fake Tavily/Exa.
type upstream struct {
	mu      sync.Mutex
	reqs    []seenReq
	handler func(n int, w http.ResponseWriter, r *http.Request, body string)
	srv     *httptest.Server
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		n := len(u.reqs)
		u.reqs = append(u.reqs, seenReq{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)})
		h := u.handler
		u.mu.Unlock()
		h(n, w, r, string(b))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) seen() []seenReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]seenReq(nil), u.reqs...)
}

type recorder struct {
	mu       sync.Mutex
	logs     []store.RequestLog
	attempts [][]store.Attempt
}

func (r *recorder) Record(l store.RequestLog, a []store.Attempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs, r.attempts = append(r.logs, l), append(r.attempts, a)
}

func (r *recorder) last(t *testing.T) (store.RequestLog, []store.Attempt) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.logs) == 0 {
		t.Fatal("no request log recorded")
	}
	return r.logs[len(r.logs)-1], r.attempts[len(r.attempts)-1]
}

type fixture struct {
	t    *testing.T
	dir  string
	conf *confstore.Manager
	pool *pool.Pool
	out  *outbound.Runtime
	rec  *recorder
	gw   *httptest.Server
	up   *upstream
}

type opts struct {
	tavilyKeys string // keys/tavily.yaml body after "keys:"
	exaKeys    string
	tokens     string // tokens.yaml; default: one token for everything
	config     map[string]any
	base       string // provider base URL override
	maxWait    time.Duration
	limits     TokenLimiter
}

func newFixture(t *testing.T, o opts) *fixture {
	t.Helper()
	dir := t.TempDir()
	w := func(name, content string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if o.tavilyKeys == "" {
		o.tavilyKeys = "  - key: tvly-key-a\n  - key: tvly-key-b\n  - key: tvly-key-c\n"
	}
	w("keys/tavily.yaml", "keys:\n"+o.tavilyKeys)
	w("keys/exa.yaml", "keys:\n"+o.exaKeys)
	if o.tokens == "" {
		o.tokens = "tokens:\n  - name: all\n    token: " + clientToken + "\n"
	}
	w("tokens.yaml", o.tokens)
	conf, err := confstore.Open(dir, confstore.Options{AdminPasswordEnv: "correct-horse-battery"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conf.SetConfig(nil, "routing.strategy", "drain-first"); err != nil { // file order: deterministic failover
		t.Fatal(err)
	}
	for path, v := range o.config {
		if _, err := conf.SetConfig(nil, path, v); err != nil {
			t.Fatal(err)
		}
	}
	up := newUpstream(t)
	base := o.base
	if base == "" {
		base = up.srv.URL
	}
	out := outbound.New(outbound.Options{})
	p := pool.New(pool.Options{EgressUsable: out.Usable})
	out.Apply(conf.Snapshot())
	p.Apply(conf.Snapshot())
	rec := &recorder{}
	r := New(Options{Conf: conf, Pool: p, Out: out, Rec: rec, MaxRateWait: o.maxWait, Limits: o.limits,
		Providers: map[string]provider.Provider{"tavily": provider.NewTavily(base), "exa": provider.NewExa(base)}})
	res, _ := httpx.NewClientIPResolver(httpx.DefaultTrustedProxies(), httpx.DefaultClientIPHeaders())
	mux := http.NewServeMux()
	mux.Handle("/tavily/", http.StripPrefix("/tavily", r.REST("tavily")))
	mux.Handle("/exa/", http.StripPrefix("/exa", r.REST("exa")))
	gw := httptest.NewServer(httpx.Chain(mux, httpx.RequestID(), httpx.ResolveClientIP(func() *httpx.ClientIPResolver { return res })))
	t.Cleanup(gw.Close)
	return &fixture{t: t, dir: dir, conf: conf, pool: p, out: out, rec: rec, gw: gw, up: up}
}

type resp struct {
	status int
	header http.Header
	body   string
}

func (f *fixture) call(method, path, body string, hdr map[string]string) resp {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.gw.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if _, ok := hdr["Authorization"]; !ok && hdr["x-api-key"] == "" && !strings.Contains(path, "=") && !strings.Contains(body, "api_key") {
		req.Header.Set("Authorization", "Bearer "+clientToken)
	}
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	r, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, r.Header, string(b)}
}

func (f *fixture) search() resp { return f.call("POST", "/tavily/search", `{"query":"golang"}`, nil) }

func (f *fixture) view(label string) pool.View {
	f.t.Helper()
	for _, v := range f.pool.Views() {
		if strings.Contains(v.Label, label) || v.ID == confstore.KeyID(v.Provider, label) {
			return v
		}
	}
	f.t.Fatalf("no key %q in %+v", label, f.pool.Views())
	return pool.View{}
}

func ok(body string) func(int, http.ResponseWriter, *http.Request, string) {
	return func(_ int, w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// byStatus answers request n with statuses[n] (last one repeats).
func byStatus(statuses []int, success string) func(int, http.ResponseWriter, *http.Request, string) {
	return func(n int, w http.ResponseWriter, _ *http.Request, _ string) {
		s := statuses[min(n, len(statuses)-1)]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s)
		if s == 200 {
			_, _ = w.Write([]byte(success))
		} else {
			fmt.Fprintf(w, `{"detail":{"error":"status %d"}}`, s)
		}
	}
}

func TestSwapsCredentialsAndFiltersHeaders(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = func(_ int, w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "upstream=1")
		w.Header().Set("X-Amzn-Trace-Id", "abc")
		w.Header().Set("X-RateLimit-Remaining", "3")
		_, _ = w.Write([]byte(`{"query":"golang","results":[],"usage":{"credits":2}}`))
	}
	r := f.call("POST", "/tavily/search", `{"query":"golang","api_key":"`+clientToken+`"}`, map[string]string{
		"Authorization": "Bearer " + clientToken, "Cookie": "panel=secret", "X-Forwarded-For": "198.51.100.50",
		"X-Real-Ip": "198.51.100.50", "Forwarded": "for=198.51.100.50", "User-Agent": "secret-agent/1.0", "Origin": "https://x.example", "X-Custom": "y",
	})
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	seen := f.up.seen()[0]
	if got := seen.header.Get("Authorization"); got != "Bearer tvly-key-a" {
		t.Errorf("upstream Authorization = %q", got)
	}
	for _, h := range []string{"Cookie", "X-Forwarded-For", "X-Real-Ip", "Forwarded", "Origin", "X-Custom", "X-Api-Key"} {
		if v := seen.header.Get(h); v != "" {
			t.Errorf("%s leaked upstream: %q", h, v)
		}
	}
	if ua := seen.header.Get("User-Agent"); ua != confstore.DefaultUserAgent {
		t.Errorf("User-Agent = %q", ua)
	}
	if strings.Contains(seen.body, clientToken) || gjson.Get(seen.body, "api_key").Exists() || !gjson.Get(seen.body, "include_usage").Bool() {
		t.Errorf("upstream body = %s", seen.body)
	}
	if strings.Contains(r.body, "usage") {
		t.Errorf("injected usage reached the client: %s", r.body)
	}
	for _, h := range []string{"Set-Cookie", "X-Amzn-Trace-Id", "X-Ratelimit-Remaining"} {
		if r.header.Get(h) != "" {
			t.Errorf("%s reached the client", h)
		}
	}
	if r.header.Get("X-Request-Id") == "" || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Errorf("response headers = %v", r.header)
	}
	l, at := f.rec.last(t)
	k := f.view("tvly-key-a")
	if l.ClientIP != "198.51.100.50" || l.IPSource != httpx.HeaderXForwardedFor || l.Provider != "tavily" || l.Surface != "rest" ||
		l.Operation != "search" || l.KeyID != k.ID || l.EgressID != "direct" || l.Attempts != 1 || l.Status != 200 ||
		l.CostCredits != 2 || l.CostEstimated || l.QueryPreview != "golang" || l.UserAgent != "secret-agent/1.0" || l.TokenID == "" || l.Outcome != "ok" {
		t.Errorf("log = %+v", l)
	}
	if at != nil {
		t.Errorf("single-attempt requests keep no attempt rows: %v", at)
	}
	if k.Used == nil || *k.Used != 2 {
		t.Errorf("credits not charged to the key: %+v", k)
	}
}

func TestClientAskedForUsageKeepsIt(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = ok(`{"results":[],"usage":{"credits":1}}`)
	r := f.call("POST", "/tavily/search", `{"query":"x","include_usage":true}`, nil)
	if !strings.Contains(r.body, `"usage"`) {
		t.Fatalf("usage the client asked for was removed: %s", r.body)
	}
}

func TestPreciseBillingOffLeavesBodyAlone(t *testing.T) {
	f := newFixture(t, opts{config: map[string]any{"upstream.precise_billing": false}})
	f.up.handler = ok(`{"results":[]}`)
	f.call("POST", "/tavily/search", `{"query":"x"}`, nil)
	if got := f.up.seen()[0].body; got != `{"query":"x"}` {
		t.Fatalf("body changed: %s", got)
	}
	l, _ := f.rec.last(t)
	if !l.CostEstimated || l.CostCredits != 1 {
		t.Fatalf("expected an estimated cost: %+v", l)
	}
}

func TestFailoverOn429(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = byStatus([]int{429, 200}, `{"results":[],"usage":{"credits":1}}`)
	r := f.search()
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	seen := f.up.seen()
	if len(seen) != 2 || seen[0].header.Get("Authorization") == seen[1].header.Get("Authorization") {
		t.Fatalf("expected two different keys: %+v", seen)
	}
	l, at := f.rec.last(t)
	if l.Attempts != 2 || len(at) != 2 || at[0].Status != 429 || at[0].ErrorKind != "rate_limited" || at[1].Status != 200 || l.ErrorKind != "" {
		t.Fatalf("log = %+v attempts = %+v", l, at)
	}
	if f.view("tvly-key-a").Status != pool.StatusCooldown {
		t.Fatalf("first key should cool down: %+v", f.view("tvly-key-a"))
	}
}

func TestBadKeysRotateAndChangeState(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = byStatus([]int{401, 432, 200}, `{"results":[],"usage":{"credits":1}}`)
	if r := f.search(); r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	if a, b := f.view("tvly-key-a"), f.view("tvly-key-b"); a.Status != pool.StatusInvalid || b.Status != pool.StatusExhausted {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = byStatus([]int{400}, "")
	r := f.search()
	if r.status != 400 || !strings.Contains(r.body, "status 400") || len(f.up.seen()) != 1 {
		t.Fatalf("status %d, %d upstream calls: %s", r.status, len(f.up.seen()), r.body)
	}
	if f.view("tvly-key-a").Status != pool.StatusActive {
		t.Fatal("a client error must not affect the key")
	}
}

func TestAllKeysRateLimitedReturnsUpstream429(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = byStatus([]int{429}, "")
	r := f.search()
	if r.status != 429 || !strings.Contains(r.body, "status 429") || r.header.Get("Retry-After") != retryAfterHint || len(f.up.seen()) != 3 {
		t.Fatalf("status %d retry-after %q calls %d: %s", r.status, r.header.Get("Retry-After"), len(f.up.seen()), r.body)
	}
}

func TestMaxAttemptsRespected(t *testing.T) {
	f := newFixture(t, opts{config: map[string]any{"routing.max_attempts": 2}})
	f.up.handler = byStatus([]int{500}, "")
	if r := f.search(); r.status != 500 || len(f.up.seen()) != 2 {
		t.Fatalf("status %d, calls %d", r.status, len(f.up.seen()))
	}
}

func TestNoKeysGivesProviderShapedError(t *testing.T) {
	f := newFixture(t, opts{})
	r := f.call("POST", "/exa/search", `{"query":"x"}`, nil)
	if r.status != 503 || gjson.Get(r.body, "tag").String() != "no_available_keys" || r.header.Get("Retry-After") == "" {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	if !strings.Contains(gjson.Get(r.body, "error").String(), "no exa keys") {
		t.Fatalf("body = %s", r.body)
	}
	l, _ := f.rec.last(t)
	if l.Status != 503 || l.ErrorKind != "no_keys" || l.Outcome != "gateway_error" {
		t.Fatalf("log = %+v", l)
	}
}

func TestExaUsesItsOwnHeaderAndCost(t *testing.T) {
	f := newFixture(t, opts{exaKeys: "  - exa-key-a\n"})
	f.up.handler = ok(`{"results":[],"costDollars":{"total":0.007}}`)
	r := f.call("POST", "/exa/search", `{"query":"x"}`, map[string]string{"x-api-key": clientToken})
	if r.status != 200 || !strings.Contains(r.body, "costDollars") {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	seen := f.up.seen()[0]
	if seen.header.Get("X-Api-Key") != "exa-key-a" || seen.header.Get("Authorization") != "" {
		t.Fatalf("upstream headers = %v", seen.header)
	}
	if l, _ := f.rec.last(t); l.CostMicroUSD != 7000 || l.CostEstimated {
		t.Fatalf("log = %+v", l)
	}
}

func TestEgressDownSkipsKeyWithoutUsingAttempts(t *testing.T) {
	proxy, _ := socks5.Start("", "")
	dead := proxy.Addr()
	proxy.Close()
	f := newFixture(t, opts{config: map[string]any{"routing.max_attempts": 1},
		tavilyKeys: "  - key: tvly-key-a\n    egress: socks5://" + dead + "\n  - key: tvly-key-b\n"})
	f.up.handler = ok(`{"results":[],"usage":{"credits":1}}`)
	r := f.search()
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	seen := f.up.seen()
	if len(seen) != 1 || seen[0].header.Get("Authorization") != "Bearer tvly-key-b" {
		t.Fatalf("only key b may reach upstream: %+v", seen)
	}
	if v := f.view("tvly-key-a"); v.Status != pool.StatusActive || v.FailScore != 0 {
		t.Fatalf("the key must not be blamed for its egress: %+v", v)
	}
	var hasErr bool
	for _, v := range f.out.Views() {
		hasErr = hasErr || (v.FailStreak == 1 && v.LastError != "")
	}
	if !hasErr {
		t.Fatalf("egress failure not recorded: %+v", f.out.Views())
	}
}

func TestOnlyKeyOnDeadEgressFailsInsteadOfGoingDirect(t *testing.T) {
	proxy, _ := socks5.Start("", "")
	dead := proxy.Addr()
	proxy.Close()
	f := newFixture(t, opts{tavilyKeys: "  - key: tvly-key-a\n    egress: socks5://" + dead + "\n"})
	f.up.handler = ok(`{"results":[]}`)
	r := f.search()
	if r.status != 503 || len(f.up.seen()) != 0 {
		t.Fatalf("status %d, upstream calls %d: %s", r.status, len(f.up.seen()), r.body)
	}
	if !strings.Contains(r.body, "could not connect through their egress") {
		t.Fatalf("the error should name the egress: %s", r.body)
	}
}

func TestRequestTravelsThroughTheKeysProxy(t *testing.T) {
	proxy, err := socks5.Start("u", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	f := newFixture(t, opts{base: "http://upstream.invalid", tavilyKeys: "  - key: tvly-key-a\n    egress: socks5://u:p@" + proxy.Addr() + "\n"})
	proxy.Redirect(func(string) string { return strings.TrimPrefix(f.up.srv.URL, "http://") })
	f.up.handler = ok(`{"results":[],"usage":{"credits":1}}`)
	if r := f.search(); r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	if got := proxy.Targets(); len(got) != 1 || got[0] != "upstream.invalid:80" {
		t.Fatalf("targets = %v", got)
	}
}

func TestEdgeBlockQuarantinesEgressNotKey(t *testing.T) {
	proxy, _ := socks5.Start("", "")
	defer proxy.Close()
	f := newFixture(t, opts{tavilyKeys: "  - key: tvly-key-a\n    egress: socks5://" + proxy.Addr() + "\n  - key: tvly-key-b\n"})
	f.up.handler = func(n int, w http.ResponseWriter, _ *http.Request, _ string) {
		if n == 0 {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`<html><center>awselb/2.0</center></html>`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"usage":{"credits":1}}`))
	}
	if r := f.search(); r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	v := f.view("tvly-key-a")
	if v.Status != pool.StatusEgressDown {
		t.Fatalf("key on a blocked egress should show egress_down: %+v", v)
	}
	if v.Reason != "" || v.FailScore != 0 {
		t.Fatalf("the key itself must stay clean: %+v", v)
	}
}

func hangUp(w http.ResponseWriter) {
	c, _, _ := w.(http.Hijacker).Hijack()
	c.Close()
}

func TestLostConnectionAfterSend(t *testing.T) {
	f := newFixture(t, opts{})
	// Search is cheap and idempotent: resend to the next key.
	f.up.handler = func(n int, w http.ResponseWriter, _ *http.Request, _ string) {
		if n == 0 {
			hangUp(w)
			return
		}
		_, _ = w.Write([]byte(`{"results":[],"usage":{"credits":1}}`))
	}
	if r := f.search(); r.status != 200 || len(f.up.seen()) < 2 {
		t.Fatalf("search should fail over: %d, calls %d", r.status, len(f.up.seen()))
	}
}

func TestResearchIsNotResentAfterSend(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = func(_ int, w http.ResponseWriter, _ *http.Request, _ string) { hangUp(w) }
	r := f.call("POST", "/tavily/research", `{"input":"x"}`, nil)
	if r.status != 502 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	if n := keysSeen(f.up.seen()); n != 1 {
		t.Fatalf("research went to %d keys: would bill twice", n)
	}
}

func keysSeen(reqs []seenReq) int {
	set := map[string]bool{}
	for _, r := range reqs {
		set[r.header.Get("Authorization")] = true
	}
	return len(set)
}

func TestTaskFollowUpUsesTheCreatingKey(t *testing.T) {
	f := newFixture(t, opts{config: map[string]any{"routing.strategy": "round-robin"}})
	f.up.handler = ok(`{"request_id":"req-42","status":"pending"}`)
	if r := f.call("POST", "/tavily/research", `{"input":"x"}`, nil); r.status != 200 {
		t.Fatalf("create: %d", r.status)
	}
	if r := f.call("GET", "/tavily/research/req-42", "", nil); r.status != 200 {
		t.Fatalf("poll: %d", r.status)
	}
	seen := f.up.seen()
	if len(seen) != 2 || seen[1].path != "/research/req-42" || seen[0].header.Get("Authorization") != seen[1].header.Get("Authorization") {
		t.Fatalf("poll must use the creating key: %+v", seen)
	}
}

func TestAuthFailures(t *testing.T) {
	tokens := "tokens:\n  - name: all\n    token: " + clientToken + "\n  - name: exa-only\n    token: exa-only-token-0123456789\n    providers: [exa]\n  - name: mcp-only\n    token: mcp-only-token-0123456789\n    surfaces: [mcp]\n"
	f := newFixture(t, opts{tokens: tokens})
	f.up.handler = ok(`{"results":[]}`)
	cases := []struct {
		name   string
		hdr    map[string]string
		status int
	}{
		{"wrong token", map[string]string{"Authorization": "Bearer wrong-token-0123456789"}, 401},
		{"other provider only", map[string]string{"Authorization": "Bearer exa-only-token-0123456789"}, 403},
		{"other surface only", map[string]string{"Authorization": "Bearer mcp-only-token-0123456789"}, 403},
	}
	for _, c := range cases {
		r := f.call("POST", "/tavily/search", `{"query":"x"}`, c.hdr)
		if r.status != c.status || gjson.Get(r.body, "detail.error").String() == "" {
			t.Errorf("%s: %d %s", c.name, r.status, r.body)
		}
	}
	req, _ := http.NewRequest("POST", f.gw.URL+"/tavily/search", strings.NewReader(`{"query":"x"}`))
	anon, _ := http.DefaultTransport.RoundTrip(req)
	anon.Body.Close()
	if anon.StatusCode != 401 {
		t.Errorf("anonymous: %d", anon.StatusCode)
	}
	if len(f.up.seen()) != 0 {
		t.Fatal("unauthenticated requests must never reach upstream")
	}
	// The token in the JSON body (older Tavily SDKs) works and is not forwarded.
	if r := f.call("POST", "/tavily/search", `{"query":"x","api_key":"`+clientToken+`"}`, nil); r.status != 200 {
		t.Fatalf("body token: %d %s", r.status, r.body)
	}
	if strings.Contains(f.up.seen()[0].body, clientToken) {
		t.Fatal("client token forwarded upstream")
	}
}

func TestPathsAllowlistAndQueryCredentials(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = ok(`{"results":[]}`)
	if r := f.call("POST", "/tavily/admin/keys", `{}`, nil); r.status != 404 || len(f.up.seen()) != 0 {
		t.Fatalf("unknown endpoint: %d", r.status)
	}
	if r := f.call("GET", "/tavily/usage", "", nil); r.status != 200 || len(f.up.seen()) != 0 {
		t.Fatalf("usage: %d (must be answered locally)", r.status)
	}
	r := f.call("POST", "/tavily/search?tavilyApiKey="+clientToken+"&foo=1", `{"query":"x"}`, nil)
	if r.status != 200 {
		t.Fatalf("query token: %d %s", r.status, r.body)
	}
	if got := f.up.seen()[0].query; got != "foo=1" {
		t.Fatalf("upstream query = %q (token must be removed)", got)
	}
}

func TestExtraHeadersAllowlist(t *testing.T) {
	f := newFixture(t, opts{config: map[string]any{"upstream.extra_headers": []string{"X-Project-Id"}}})
	f.up.handler = ok(`{"results":[]}`)
	f.call("POST", "/tavily/search", `{"query":"x"}`, map[string]string{"X-Project-Id": "p1", "X-Other": "no"})
	h := f.up.seen()[0].header
	if h.Get("X-Project-Id") != "p1" || h.Get("X-Other") != "" {
		t.Fatalf("headers = %v", h)
	}
}

func TestUpstreamErrorNeverEchoesKey(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = func(_ int, w http.ResponseWriter, _ *http.Request, _ string) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"detail":{"error":"bad request for key tvly-key-a"}}`))
	}
	if r := f.search(); strings.Contains(r.body, "tvly-key-a") || !strings.Contains(r.body, "[key]") {
		t.Fatalf("body = %s", r.body)
	}
}

func TestEventStreamIsPassedThroughLive(t *testing.T) {
	f := newFixture(t, opts{})
	release := make(chan struct{})
	f.up.handler = func(_ int, w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "data: second\n\n")
	}
	req, _ := http.NewRequest("POST", f.gw.URL+"/tavily/research", strings.NewReader(`{"input":"x","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+clientToken)
	r, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	br := bufio.NewReader(r.Body)
	line, _ := br.ReadString('\n')
	if line != "data: first\n" {
		t.Fatalf("first line = %q", line)
	}
	close(release) // the first event arrived before upstream finished: no buffering
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "data: second") {
		t.Fatalf("rest = %q", rest)
	}
	time.Sleep(50 * time.Millisecond) // the handler records after the stream ends
	if l, _ := f.rec.last(t); l.Status != 200 || !l.CostEstimated || l.CostCredits == 0 {
		t.Fatalf("log = %+v", l)
	}
}

func TestDeadlineReturns504(t *testing.T) {
	f := newFixture(t, opts{config: map[string]any{"routing.deadline_seconds": 10}})
	r := New(Options{Conf: f.conf, Pool: f.pool, Out: f.out, Rec: f.rec,
		Providers: map[string]provider.Provider{"tavily": provider.NewTavily(f.up.srv.URL)},
		Now: func() func() time.Time {
			t0 := time.Now()
			n := 0
			return func() time.Time { n++; return t0.Add(time.Duration(n) * 11 * time.Second) }
		}()})
	tok := f.conf.Snapshot().Tokens[0]
	p := provider.NewTavily(f.up.srv.URL)
	op, _ := p.Match("POST", "/search")
	res := r.Do(context.Background(), &Call{Provider: p, Op: op, Body: []byte(`{}`), RawBody: []byte(`{}`), Token: &tok, Surface: "rest"})
	if res.Status != http.StatusGatewayTimeout || !res.Gateway || len(f.up.seen()) != 0 {
		t.Fatalf("result = %+v, upstream calls %d", res, len(f.up.seen()))
	}
}

func TestKeyGroupRestriction(t *testing.T) {
	tokens := "tokens:\n  - name: paid-only\n    token: " + clientToken + "\n    key_group: paid\n"
	f := newFixture(t, opts{tokens: tokens, tavilyKeys: "  - key: tvly-free\n  - key: tvly-paid\n    plan: paid\n"})
	f.up.handler = ok(`{"results":[]}`)
	f.search()
	if got := f.up.seen()[0].header.Get("Authorization"); got != "Bearer tvly-paid" {
		t.Fatalf("key_group ignored: %s", got)
	}
}

func TestPreviewLimitFromConfig(t *testing.T) {
	f := newFixture(t, opts{config: map[string]any{"logs.query_preview_chars": 5}})
	f.up.handler = ok(`{"results":[]}`)
	f.call("POST", "/tavily/search", `{"query":"0123456789"}`, nil)
	if l, _ := f.rec.last(t); l.QueryPreview != "01234" {
		t.Fatalf("preview = %q", l.QueryPreview)
	}
}
