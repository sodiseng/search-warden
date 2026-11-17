package quota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
)

// upstream is a fake Tavily GET /usage.
type upstream struct {
	srv     *httptest.Server
	mu      sync.Mutex
	status  map[string]int // key → status (default 200)
	used    map[string]int
	calls   []string
	headers []http.Header
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{status: map[string]int{}, used: map[string]int{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasPrefix(r.URL.Path, "/agent/runs/") { // Exa key probe
			key = r.Header.Get("x-api-key")
			u.mu.Lock()
			u.calls = append(u.calls, key)
			u.mu.Unlock()
			if key == "exa-bad" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"Invalid API key.","tag":"INVALID_API_KEY"}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"statusCode":404,"error":{"type":"NOT_FOUND","code":"RUN_NOT_FOUND"}}`)
			return
		}
		u.mu.Lock()
		u.calls = append(u.calls, key)
		u.headers = append(u.headers, r.Header.Clone())
		status, used := u.status[key], u.used[key]
		u.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/usage" {
			http.NotFound(w, r)
			return
		}
		switch status {
		case 0, http.StatusOK:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"key":{"usage":%d,"limit":null},"account":{"current_plan":"Researcher","plan_usage":%d,"plan_limit":1000,"paygo_usage":0,"paygo_limit":null}}`, used, used)
		case http.StatusForbidden:
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "<html><body><center>awselb/2.0</center></body></html>")
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"detail":{"error":"nope"}}`)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstream) callLog() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.calls...)
}

// fakeOut stands in for the egress runtime.
type fakeOut struct {
	mu       sync.Mutex
	client   *http.Client
	ips      map[string]string
	down     map[string]bool
	connFail []string
	blocked  []string
}

func (f *fakeOut) Client(string) *http.Client { return f.client }
func (f *fakeOut) Usable(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.down[id]
}
func (f *fakeOut) ExitIP(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ips[id]
}
func (f *fakeOut) ReportConnectFailure(id string, _ error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connFail = append(f.connFail, id)
}
func (f *fakeOut) ReportSuccess(string) {}
func (f *fakeOut) MarkBlocked(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocked = append(f.blocked, id)
}

type env struct {
	t     *testing.T
	clock time.Time
	up    *upstream
	out   *fakeOut
	pool  *pool.Pool
	snap  *confstore.Snapshot
	s     *Syncer
}

func tavilyKey(secret, egressID string) confstore.Key {
	return confstore.Key{ID: confstore.KeyID("tavily", secret), Provider: "tavily", Secret: secret, Label: secret, Plan: "free",
		ResetDay: 1, Enabled: true, Egress: egress.Resolved{ID: egressID, Enabled: true}}
}

func newEnv(t *testing.T, keys ...confstore.Key) *env {
	e := &env{t: t, clock: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), up: newUpstream(t)}
	e.out = &fakeOut{client: e.up.srv.Client(), ips: map[string]string{"eg-a": "198.51.100.1", "eg-b": "198.51.100.2"}, down: map[string]bool{}}
	e.snap = &confstore.Snapshot{Keys: keys, Config: confstore.Config{
		Quota: confstore.QuotaConfig{Sync: true, HotIntervalMinutes: 15, ColdIntervalHours: 24}}}
	e.pool = pool.New(pool.Options{Now: e.now})
	e.pool.Apply(e.snap)
	e.s = New(e.opts(-1))
	return e
}

func (e *env) now() time.Time { return e.clock }

func (e *env) opts(startDelay time.Duration) Options {
	return Options{Snapshot: func() *confstore.Snapshot { return e.snap }, Pool: e.pool, Out: e.out, Now: e.now, StartDelay: startDelay,
		Providers: map[string]provider.Provider{"tavily": provider.NewTavily(e.up.srv.URL), "exa": provider.NewExa(e.up.srv.URL)}}
}

func (e *env) step() string {
	id, _ := e.s.Step(context.Background())
	return id
}

func (e *env) advance(d time.Duration) { e.clock = e.clock.Add(d) }

func (e *env) view(id string) pool.View {
	e.t.Helper()
	for _, v := range e.pool.Views() {
		if v.ID == id {
			return v
		}
	}
	e.t.Fatalf("no view for %s", id)
	return pool.View{}
}

func (e *env) expect(want string) {
	e.t.Helper()
	if got := e.step(); got != want {
		e.t.Fatalf("synced %q, want %q (calls so far: %v)", got, want, e.up.callLog())
	}
}

func TestGatesAndOrder(t *testing.T) {
	a, b, c := tavilyKey("tvly-a", "eg-a"), tavilyKey("tvly-b", "eg-a"), tavilyKey("tvly-c", "eg-b")
	x := confstore.Key{ID: confstore.KeyID("exa", "exa-x"), Provider: "exa", Secret: "exa-x", Plan: "free", ResetDay: 1, Enabled: true,
		Egress: egress.Resolved{ID: "eg-a", Enabled: true}}
	e := newEnv(t, a, b, c, x)
	e.s = New(e.opts(0)) // default start delay: one gap

	e.expect("") // the exit-IP gate is not persisted, so nothing goes out right after a start
	e.advance(DefaultGap)
	e.expect(a.ID) // never synced, first in the file
	e.expect(c.ID) // b leaves from a's exit IP, which was just used
	e.expect("")   // b waits for the IP gate, a and c for their own gap; Exa has no balance API
	e.advance(DefaultGap - time.Second)
	e.expect("")
	e.advance(time.Second)
	e.expect(b.ID)
	e.expect("")

	v := e.view(a.ID)
	if v.UpstreamPlan != "Researcher" || *v.Limit != 1000 || *v.Remaining != 1000 || v.LastSyncedAt.IsZero() {
		t.Fatalf("view = %+v", v)
	}
	if got := e.up.callLog(); strings.Join(got, ",") != "tvly-a,tvly-c,tvly-b" {
		t.Fatalf("calls = %v", got)
	}
	for _, h := range e.up.headers {
		if h.Get("User-Agent") != confstore.DefaultUserAgent || h.Get("X-Forwarded-For") != "" {
			t.Fatalf("headers = %v", h)
		}
	}
}

func TestExitIPSharedAcrossEgresses(t *testing.T) {
	a, c := tavilyKey("tvly-a", "eg-a"), tavilyKey("tvly-c", "eg-b")
	e := newEnv(t, a, c)
	e.out.ips["eg-b"] = e.out.ips["eg-a"] // two nodes, one exit IP: one gate
	e.expect(a.ID)
	e.expect("")
	e.advance(DefaultGap)
	e.expect(c.ID)

	// While an egress's IP is unknown, the egress itself is the gate.
	d := tavilyKey("tvly-d", "eg-unknown")
	e2 := newEnv(t, a, d)
	e2.expect(a.ID)
	e2.expect(d.ID)
}

func TestHotColdAndManual(t *testing.T) {
	a, b := tavilyKey("tvly-a", "eg-a"), tavilyKey("tvly-b", "eg-b")
	e := newEnv(t, a, b)
	e.expect(a.ID)
	e.expect(b.ID)

	e.advance(16 * time.Minute)
	e.expect("") // neither key was used since: nothing to learn
	l, err := e.pool.Pick(pool.Request{Provider: "tavily", Exclude: map[string]bool{b.ID: true}})
	if err != nil {
		t.Fatal(err)
	}
	l.Done(provider.Outcome{Class: provider.ClassOK, Status: 200}, provider.Cost{Credits: 1})
	e.advance(time.Minute)
	e.expect(a.ID) // hot: used within 2 h, last synced over 15 min ago
	e.expect("")

	e.advance(24 * time.Hour)
	e.expect(b.ID) // cold, and the older sync goes first
	e.expect(a.ID)

	// Background sync off: nothing is due, but a manual request still goes out.
	e.snap.Config.Quota.Sync = false
	e.advance(48 * time.Hour)
	e.expect("")
	if n := e.s.Request(b.ID); n != 1 {
		t.Fatalf("queued %d", n)
	}
	e.expect(b.ID)
	if n := e.s.Request(); n != 2 {
		t.Fatalf("queued %d", n)
	}
	e.expect(a.ID) // b was synced a moment ago and waits for its gap
	e.expect("")
	e.advance(DefaultGap)
	e.expect(b.ID)
}

func TestFailuresBackOff(t *testing.T) {
	a := tavilyKey("tvly-a", "eg-a")
	e := newEnv(t, a)

	e.up.status["tvly-a"] = http.StatusTooManyRequests
	e.expect(a.ID)
	if v := e.view(a.ID); v.SyncError != "rate limited (429)" || !v.NextSyncAt.Equal(e.clock.Add(5*time.Minute)) || v.Status != pool.StatusActive {
		t.Fatalf("after 1st 429: %+v", v)
	}
	e.advance(5 * time.Minute)
	e.expect(a.ID)
	if v := e.view(a.ID); !v.NextSyncAt.Equal(e.clock.Add(10 * time.Minute)) {
		t.Fatalf("after 2nd 429: next %v", v.NextSyncAt.Sub(e.clock))
	}

	// 401: the key is invalid, and retested a day later.
	e.up.status["tvly-a"] = http.StatusUnauthorized
	e.advance(10 * time.Minute)
	e.expect(a.ID)
	if v := e.view(a.ID); v.Status != pool.StatusInvalid || !v.NextSyncAt.Equal(e.clock.Add(24*time.Hour)) {
		t.Fatalf("after 401: %+v", v)
	}
	e.up.status["tvly-a"] = http.StatusOK
	e.advance(24 * time.Hour)
	e.expect(a.ID)
	if v := e.view(a.ID); v.Status != pool.StatusActive || v.SyncError != "" {
		t.Fatalf("after the retest: %+v", v)
	}

	// An HTML 403 means the egress IP is blocked at upstream's edge.
	e.up.status["tvly-a"] = http.StatusForbidden
	e.advance(25 * time.Hour)
	e.expect(a.ID)
	if len(e.out.blocked) != 1 || e.out.blocked[0] != "eg-a" || e.view(a.ID).Status != pool.StatusActive {
		t.Fatalf("blocked = %v, view %+v", e.out.blocked, e.view(a.ID))
	}

	// A key whose egress is down is never synced another way.
	e.out.down["eg-a"] = true
	e.advance(48 * time.Hour)
	e.expect("")
}

func TestConnectFailureBlamesEgressAndRedacts(t *testing.T) {
	a := tavilyKey("tvly-secret-a", "eg-a")
	e := newEnv(t, a)
	e.out.client = &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("socks connect via socks5://user:tvly-secret-a@203.0.113.9:1080 failed")
	}}}
	e.expect(a.ID)
	v := e.view(a.ID)
	if len(e.out.connFail) != 1 || !strings.HasPrefix(v.SyncError, "request failed") {
		t.Fatalf("connect failures %v, view %+v", e.out.connFail, v)
	}
	if strings.Contains(v.SyncError, "tvly-secret-a") || strings.Contains(v.SyncError, "user:") {
		t.Fatalf("secret leaked: %q", v.SyncError)
	}
	if v.Status != pool.StatusActive {
		t.Fatalf("an egress failure must not change the key: %+v", v)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	a := tavilyKey("tvly-a", "eg-a")
	e := newEnv(t, a)
	e.s = New(func() Options { o := e.opts(-1); o.Tick = 10 * time.Millisecond; return o }())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.s.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for len(e.up.callLog()) == 0 {
		select {
		case <-deadline:
			t.Fatal("Run never synced")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func exaKey(secret string) confstore.Key {
	return confstore.Key{ID: confstore.KeyID("exa", secret), Provider: "exa", Secret: secret, Label: secret, Plan: "free",
		ResetDay: 1, Enabled: true, Egress: egress.Resolved{ID: "eg-b", Enabled: true}}
}

func TestKeyTest(t *testing.T) {
	tv, good, bad := tavilyKey("tvly-a", "eg-a"), exaKey("exa-good"), exaKey("exa-bad")
	e := newEnv(t, tv, good, bad)
	e.s.Request() // nothing due for Exa; this only queues the Tavily key
	e.pool.SyncFailed(good.ID, provider.Outcome{Class: provider.ClassKeyInvalid, Status: 401}, "invalid key (401)")
	if n := e.s.Test(); n != 3 {
		t.Fatalf("queued %d", n)
	}
	e.s.runProbes(context.Background())
	if v := e.view(good.ID); v.Status != pool.StatusActive || v.SyncError != "" {
		t.Fatalf("good key after test = %+v", v) // an accepted key comes back from invalid
	}
	if v := e.view(bad.ID); v.Status != pool.StatusInvalid || !strings.Contains(v.SyncError, "401") {
		t.Fatalf("bad key after test = %+v", v)
	}
	e.expect(tv.ID) // the Tavily key is tested by a quota sync
	// A key whose egress is down is not tested another way.
	e.out.down["eg-b"] = true
	e.s.Test(good.ID)
	before := len(e.up.callLog())
	e.s.runProbes(context.Background())
	if len(e.up.callLog()) != before || !strings.Contains(e.view(good.ID).SyncError, "egress") {
		t.Fatal("tested through an unusable egress")
	}
}
