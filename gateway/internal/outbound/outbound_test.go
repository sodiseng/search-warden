package outbound

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/store"
	"github.com/sodiseng/search-warden/gateway/internal/testutil/socks5"
)

// dialLog records every TCP dial the runtime makes itself.
type dialLog struct {
	mu    sync.Mutex
	addrs []string
}

func (d *dialLog) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, addr)
}

func (d *dialLog) contains(addr string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.addrs {
		if a == addr {
			return true
		}
	}
	return false
}

func registry(t *testing.T, nodes ...egress.Node) *egress.Registry {
	t.Helper()
	r, errs := egress.NewRegistry(nodes)
	if errs != nil {
		t.Fatal(errs)
	}
	return r
}

func snapshot(t *testing.T, reg *egress.Registry, keys ...confstore.Key) *confstore.Snapshot {
	t.Helper()
	s := &confstore.Snapshot{Egresses: reg, Keys: keys}
	s.Config.Egress.HealthIntervalSeconds = 1
	return s
}

func testKey(t *testing.T, reg *egress.Registry, secret, ref string) confstore.Key {
	t.Helper()
	res, err := reg.Resolve(ref)
	if err != nil {
		t.Fatal(err)
	}
	return confstore.Key{ID: confstore.KeyID("tavily", secret), Provider: "tavily", Secret: secret, Enabled: true, EgressRef: ref, Egress: res}
}

func upstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("203.0.113.9")) }))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, c *http.Client, url string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestSocksEgressUsesRemoteDNSAndNeverDialsUpstream(t *testing.T) {
	up := upstream(t)
	upHost := strings.TrimPrefix(up.URL, "http://")
	proxy, err := socks5.Start("user", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	proxy.Redirect(func(string) string { return upHost })

	reg := registry(t, egress.Node{Name: "hk-1", URL: "socks5://user:secret@" + proxy.Addr(), Enabled: true})
	dl := &dialLog{}
	r := New(Options{DialContext: dl.dial})
	k := testKey(t, reg, "tvly-a", "hk-1")
	r.Apply(snapshot(t, reg, k))

	if err := get(t, r.Client(k.ID), "http://upstream.invalid:8080/search"); err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	if got := proxy.Targets(); len(got) != 1 || got[0] != "upstream.invalid:8080" {
		t.Fatalf("proxy must receive the hostname (remote DNS), got %v", got)
	}
	if !dl.contains(proxy.Addr()) || dl.contains("upstream.invalid:8080") || dl.contains(upHost) {
		t.Fatalf("dials = %v: only the proxy may be dialed", dl.addrs)
	}
}

func TestIgnoresSystemProxyEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	up := upstream(t)
	reg := registry(t)
	r := New(Options{})
	k := testKey(t, reg, "tvly-direct", "direct")
	r.Apply(snapshot(t, reg, k))
	if err := get(t, r.Client(k.ID), up.URL); err != nil {
		t.Fatalf("direct key must not use the system proxy: %v", err)
	}
}

func TestProxyDownNeverFallsBackToDirect(t *testing.T) {
	up := upstream(t)
	upHost := strings.TrimPrefix(up.URL, "http://")
	proxy, _ := socks5.Start("", "")
	proxyAddr := proxy.Addr()
	proxy.Close() // outage

	reg := registry(t, egress.Node{Name: "hk-1", URL: "socks5://" + proxyAddr, Enabled: true})
	dl := &dialLog{}
	r := New(Options{DialContext: dl.dial})
	k := testKey(t, reg, "tvly-a", "hk-1")
	r.Apply(snapshot(t, reg, k))

	if err := get(t, r.Client(k.ID), up.URL); err == nil {
		t.Fatal("request succeeded although the proxy is down")
	}
	if dl.contains(upHost) {
		t.Fatalf("upstream was dialed directly: %v", dl.addrs)
	}
}

func TestEachKeyHasItsOwnConnections(t *testing.T) {
	up := upstream(t)
	upHost := strings.TrimPrefix(up.URL, "http://")
	proxy, _ := socks5.Start("", "")
	defer proxy.Close()
	proxy.Redirect(func(string) string { return upHost })
	reg := registry(t, egress.Node{Name: "hk-1", URL: "socks5://" + proxy.Addr(), Enabled: true})
	r := New(Options{})
	a, b := testKey(t, reg, "tvly-a", "hk-1"), testKey(t, reg, "tvly-b", "hk-1")
	r.Apply(snapshot(t, reg, a, b))
	if r.Client(a.ID) == r.Client(b.ID) {
		t.Fatal("keys share a client")
	}
	for i := 0; i < 3; i++ { // keep-alive: repeated use of one key reuses its own connection
		_ = get(t, r.Client(a.ID), up.URL)
		_ = get(t, r.Client(b.ID), up.URL)
	}
	if n := proxy.Conns(); n < 2 {
		t.Fatalf("expected separate connections per key, proxy saw %d", n)
	}
}

func TestApplyRebuildsClientWhenEgressChanges(t *testing.T) {
	reg := registry(t, egress.Node{Name: "hk-1", URL: "socks5://127.0.0.1:9", Enabled: true})
	r := New(Options{})
	k := testKey(t, reg, "tvly-a", "direct")
	r.Apply(snapshot(t, reg, k))
	c1 := r.Client(k.ID)
	r.Apply(snapshot(t, reg, k))
	if r.Client(k.ID) != c1 {
		t.Fatal("unchanged egress must keep the client")
	}
	k2 := testKey(t, reg, "tvly-a", "hk-1")
	r.Apply(snapshot(t, reg, k2))
	if r.Client(k.ID) == c1 {
		t.Fatal("egress change must rebuild the client")
	}
	r.Apply(snapshot(t, reg))
	if r.Client(k.ID) != nil {
		t.Fatal("removed key must lose its client")
	}
}

func runtimeFor(t *testing.T, targets []string, echo []string, clock func() time.Time) *Runtime {
	t.Helper()
	return New(Options{ProbeTargets: targets, IPEchoURLs: echo, Now: clock})
}

func TestProbeHealthyDetectsExitIP(t *testing.T) {
	up := upstream(t)
	reg := registry(t)
	r := runtimeFor(t, []string{up.URL}, []string{up.URL}, nil)
	r.Apply(snapshot(t, reg, testKey(t, reg, "tvly-a", "direct")))
	for _, j := range r.due() {
		r.probeEgress(context.Background(), j)
	}
	v := r.Views()[0]
	if v.Health != HealthHealthy || v.ExitIP != "203.0.113.9" || v.ID != egress.DirectID {
		t.Fatalf("view = %+v", v)
	}
	if !r.Usable(egress.DirectID) || r.ExitIP(egress.DirectID) != "203.0.113.9" {
		t.Fatal("direct should be usable with its exit IP")
	}
}

func TestQuarantineAfterConnectFailuresAndRecovery(t *testing.T) {
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	reg := registry(t, egress.Node{Name: "hk-1", URL: "socks5://127.0.0.1:9", Enabled: true})
	up := upstream(t)
	r := runtimeFor(t, []string{up.URL}, nil, func() time.Time { return clock })
	k := testKey(t, reg, "tvly-a", "hk-1")
	r.Apply(snapshot(t, reg, k))
	id := k.Egress.ID

	boom := errors.New("socks connect tcp socks5://user:pw@1.1.1.1:1080: refused")
	r.ReportConnectFailure(id, boom)
	r.ReportConnectFailure(id, boom)
	if !r.Usable(id) {
		t.Fatal("two failures must not quarantine")
	}
	r.ReportConnectFailure(id, boom)
	if r.Usable(id) {
		t.Fatal("three failures must quarantine")
	}
	v := r.Views()[1]
	if v.Health != HealthQuarantined || strings.Contains(v.LastError, "pw") {
		t.Fatalf("view = %+v (credentials must be scrubbed)", v)
	}
	// Not due until the backoff has passed.
	if jobs := r.due(); len(jobs) != 1 || jobs[0].st.id != egress.DirectID {
		t.Fatalf("quarantined egress probed too early: %d jobs", len(jobs))
	}
	r.eg[egress.DirectID].probing = false
	clock = clock.Add(backoffMin + time.Second)

	// Stale probe result (epoch moved) must not clear a newer quarantine.
	var job *probeJob
	for _, j := range r.due() {
		if j.st.id == id {
			job = j
		}
	}
	if job == nil {
		t.Fatal("egress not due after backoff")
	}
	r.mu.Lock()
	r.eg[id].epoch++
	r.mu.Unlock()
	// The real proxy is unreachable anyway; simulate success via a working client.
	job.client = http.DefaultClient
	r.probeEgress(context.Background(), job)
	if r.Usable(id) {
		t.Fatal("stale probe overwrote the quarantine")
	}

	// A fresh successful probe recovers the egress.
	clock = clock.Add(2 * time.Second)
	r.mu.Lock()
	r.eg[id].nextProbe = clock
	r.mu.Unlock()
	for _, j := range r.due() {
		if j.st.id == id {
			j.client = http.DefaultClient
			r.probeEgress(context.Background(), j)
		}
	}
	if !r.Usable(id) {
		t.Fatalf("egress did not recover: %+v", r.Views()[1])
	}
}

func TestEdgeBlockPausesEgress(t *testing.T) {
	reg := registry(t, egress.Node{Name: "hk-1", URL: "socks5://127.0.0.1:9", Enabled: true})
	r := New(Options{})
	k := testKey(t, reg, "tvly-a", "hk-1")
	r.Apply(snapshot(t, reg, k))
	r.MarkBlocked(k.Egress.ID)
	if r.Usable(k.Egress.ID) {
		t.Fatal("blocked egress must not be usable")
	}
	if v := r.Views()[1]; v.Health != HealthBlocked || time.Until(v.Until) < 5*time.Hour {
		t.Fatalf("view = %+v", v)
	}
}

func TestDisabledNodeIsNotUsable(t *testing.T) {
	reg := registry(t, egress.Node{Name: "off", URL: "socks5://127.0.0.1:9", Enabled: false})
	r := New(Options{})
	k := testKey(t, reg, "tvly-a", "off")
	r.Apply(snapshot(t, reg, k))
	if r.Usable(k.Egress.ID) || r.Usable("no-such-egress") {
		t.Fatal("disabled or unknown egress must not be usable")
	}
}

func TestPersistence(t *testing.T) {
	st, err := store.Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(t.TempDir(), "db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	reg := registry(t, egress.Node{Name: "hk-1", URL: "socks5://127.0.0.1:9", Enabled: true})
	k := testKey(t, reg, "tvly-a", "hk-1")
	r := New(Options{Store: st})
	r.Apply(snapshot(t, reg, k))
	r.MarkBlocked(k.Egress.ID)
	if err := r.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	r2 := New(Options{Store: st})
	if err := r2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	r2.Apply(snapshot(t, reg, k))
	if r2.Usable(k.Egress.ID) {
		t.Fatal("a blocked egress must stay blocked across restarts")
	}
	// Removing the key and node drops the stored state.
	empty := registry(t)
	r2.Apply(snapshot(t, empty))
	if err := r2.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, _ := st.ListEgressStates(context.Background())
	if _, ok := m[k.Egress.ID]; ok {
		t.Fatal("state of a removed egress must be deleted")
	}
}

func TestParseIP(t *testing.T) {
	for in, want := range map[string]string{"1.2.3.4\n": "1.2.3.4", "fl=1\nip=2001:db8::1\nts=2\n": "2001:db8::1"} {
		if got, err := parseIP(in); err != nil || got != want {
			t.Errorf("parseIP(%q) = %q %v", in, got, err)
		}
	}
	if _, err := parseIP("<html>"); err == nil {
		t.Error("garbage accepted")
	}
}
