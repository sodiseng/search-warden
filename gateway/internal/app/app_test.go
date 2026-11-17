package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/obs"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// testDeps opens a confstore and SQLite store in a fresh temp data dir.
func testDeps(t *testing.T) (Deps, string) {
	t.Helper()
	dir := t.TempDir()
	conf, err := confstore.Open(dir, confstore.Options{AdminPasswordEnv: "correct-horse-battery"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(dir, "db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	out := outbound.New(outbound.Options{ProbeTargets: []string{"http://127.0.0.1:1/"}, IPEchoURLs: []string{"http://127.0.0.1:1/"}})
	keys := pool.New(pool.Options{Store: st, EgressUsable: out.Usable})
	out.Apply(conf.Snapshot())
	keys.Apply(conf.Snapshot())
	return Deps{Conf: conf, Store: st, Pool: keys, Out: out, Rec: obs.NewWriter(st, nil)}, dir
}

func mustApp(t *testing.T, env map[string]string) *App {
	t.Helper()
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	deps, _ := testDeps(t)
	a, err := New(cfg, slog.New(slog.DiscardHandler), deps)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestGatewayRoutes(t *testing.T) {
	a := mustApp(t, nil)
	cases := map[string]int{
		"/healthz":       http.StatusOK,
		"/tavily/mcp":    http.StatusMethodNotAllowed, // MCP is POST only (no GET stream, no sessions)
		"/tavily/mcp/":   http.StatusMethodNotAllowed, // trailing slash tolerated
		"/exa/mcp":       http.StatusMethodNotAllowed,
		"/mcp":           http.StatusMethodNotAllowed,
		"/tavily/search": http.StatusUnauthorized, // REST passthrough requires a token
		"/exa/contents":  http.StatusUnauthorized,
		"/admin/":        http.StatusUnauthorized, // the panel shares the gateway port by default
		"/admin/login":   http.StatusOK,           // its login page is public
		"/":              http.StatusForbidden,    // the bare domain gives nothing away
		"/nope":          http.StatusNotFound,
	}
	for p, want := range cases {
		rec := do(a.Gateway, http.MethodGet, p)
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", p, rec.Code, want)
		}
		if rec.Header().Get("X-Request-Id") == "" {
			t.Errorf("GET %s: missing X-Request-Id", p)
		}
	}
}

func TestSeparateAdmin(t *testing.T) {
	a := mustApp(t, map[string]string{"WARDEN_ADMIN_LISTEN": ":18684"})
	if c := do(a.Admin, http.MethodGet, "/").Code; c != http.StatusUnauthorized {
		t.Fatalf("admin / without session = %d, want 401", c)
	}
	rec := do(a.Admin, http.MethodGet, "/login")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatalf("admin /login = %d, CSP %q", rec.Code, rec.Header().Get("Content-Security-Policy"))
	}
	if do(a.Admin, http.MethodGet, "/healthz").Code != http.StatusOK {
		t.Error("admin /healthz")
	}
}

func TestSeparateAdminWithBasePath(t *testing.T) {
	a := mustApp(t, map[string]string{"WARDEN_ADMIN_LISTEN": ":18684", "WARDEN_ADMIN_BASE_PATH": "/warden-admin"})
	if c := do(a.Admin, http.MethodGet, "/warden-admin/").Code; c != http.StatusUnauthorized {
		t.Errorf("base path page without session = %d", c)
	}
	if c := do(a.Admin, http.MethodGet, "/warden-admin/login").Code; c != http.StatusOK {
		t.Errorf("login under base path = %d", c)
	}
	if c := do(a.Admin, http.MethodGet, "/").Code; c != http.StatusForbidden {
		t.Errorf("root outside base path = %d", c)
	}
	if c := do(a.Admin, http.MethodGet, "/nope").Code; c != http.StatusNotFound {
		t.Errorf("unknown path outside base path = %d", c)
	}
	if c := do(a.Admin, http.MethodGet, "/healthz").Code; c != http.StatusOK {
		t.Errorf("healthz outside base path = %d", c)
	}
}

func TestSharedAdmin(t *testing.T) {
	a := mustApp(t, map[string]string{"WARDEN_ADMIN_LISTEN": "shared"})
	if a.Admin != nil {
		t.Fatal("shared mode must not create a separate admin handler")
	}
	if c := do(a.Gateway, http.MethodGet, "/admin/").Code; c != http.StatusUnauthorized {
		t.Fatalf("/admin/ without session = %d", c)
	}
	rec := do(a.Gateway, http.MethodGet, "/admin/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("/admin/login = %d", rec.Code)
	}
	// Admin CSP must override the gateway's default-src 'none'.
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("shared admin CSP = %q", csp)
	}
	if do(a.Gateway, http.MethodGet, "/admin").Code != http.StatusMovedPermanently {
		t.Error("/admin should redirect to /admin/")
	}
	if do(a.Gateway, http.MethodPost, "/tavily/mcp").Code != http.StatusUnauthorized {
		t.Error("gateway routes must still work in shared mode")
	}
}

// A domain opened without a path answers 403 and names no route, port or
// product, whatever the method or query string.
func TestBareDomainIsForbidden(t *testing.T) {
	shared := mustApp(t, nil)
	split := mustApp(t, map[string]string{"WARDEN_ADMIN_LISTEN": ":18684", "WARDEN_ADMIN_BASE_PATH": "/warden-admin"})
	handlers := map[string]http.Handler{
		"gateway sharing its port with the panel": shared.Gateway,
		"gateway with a separate panel port":      split.Gateway,
		"panel port with the panel on a subpath":  split.Admin,
	}
	for name, h := range handlers {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			for _, target := range []string{"/", "/?tavilyApiKey=x"} {
				rec := do(h, method, target)
				body := strings.TrimSpace(rec.Body.String())
				if rec.Code != http.StatusForbidden || body != `{"error":"forbidden","message":"forbidden"}` {
					t.Errorf("%s: %s %s = %d %s, want a bare 403", name, method, target, rec.Code, body)
				}
			}
		}
	}
}

func TestEnsureDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	if err := EnsureDataDir(dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("data dir not created: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("write-test file left behind: %v", entries)
	}
	// A directory created by mkdir or Docker (0755) is tightened: it holds plaintext keys (§11.5).
	if runtime.GOOS != "windows" {
		loose := filepath.Join(t.TempDir(), "loose")
		if err := os.Mkdir(loose, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := EnsureDataDir(loose); err != nil {
			t.Fatal(err)
		}
		if fi, _ := os.Stat(loose); fi.Mode().Perm() != 0o700 {
			t.Errorf("data dir mode = %v, want 0700", fi.Mode().Perm())
		}
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	free := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		return ln.Addr().String()
	}
	gwAddr, adAddr := free(), free()
	a := mustApp(t, map[string]string{"WARDEN_LISTEN": gwAddr, "WARDEN_ADMIN_LISTEN": adAddr})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	get := func(addr string) (int, error) {
		var lastErr error
		for i := 0; i < 50; i++ {
			resp, err := http.Get("http://" + addr + "/healthz")
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return resp.StatusCode, nil
			}
			lastErr = err
			time.Sleep(20 * time.Millisecond)
		}
		return 0, lastErr
	}
	for _, addr := range []string{gwAddr, adAddr} {
		if code, err := get(addr); err != nil || code != http.StatusOK {
			t.Fatalf("GET %s/healthz: %d %v", addr, code, err)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not shut down")
	}
}

func TestConfigHotReloadReachesRequestPath(t *testing.T) {
	deps, dir := testDeps(t)
	conf := deps.Conf
	var buf bytes.Buffer
	cfg, _ := config.Load(func(string) string { return "" })
	a, err := New(cfg, slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), deps)
	if err != nil {
		t.Fatal(err)
	}
	probe := func() string {
		buf.Reset()
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		r.RemoteAddr = "127.0.0.1:5000"
		r.Header.Set("X-Forwarded-For", "198.51.100.23")
		rec := httptest.NewRecorder()
		a.Gateway.ServeHTTP(rec, r)
		return rec.Body.String()
	}
	// Default config trusts loopback → client IP comes from X-Forwarded-For.
	probe()
	if !strings.Contains(buf.String(), `"client_ip":"198.51.100.23"`) {
		t.Fatalf("default trust not applied: %s", buf.String())
	}
	// Hand-edit config.yaml to trust only 10.0.0.0/8; after reload, loopback is no longer trusted.
	p := filepath.Join(dir, "config.yaml")
	b, _ := os.ReadFile(p)
	edited := strings.Replace(string(b), `trusted_proxies: [127.0.0.0/8, "::1/128", 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, "fc00::/7"]`, `trusted_proxies: [10.0.0.0/8]`, 1)
	if edited == string(b) {
		t.Fatal("template line not found")
	}
	if err := os.WriteFile(p, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	conf.Reload()
	probe()
	if !strings.Contains(buf.String(), `"client_ip":"127.0.0.1"`) {
		t.Fatalf("reloaded trust not applied: %s", buf.String())
	}
	// A broken edit keeps serving but reports degraded health.
	if err := os.WriteFile(p, []byte("routing: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conf.Reload()
	if body := probe(); !strings.Contains(body, `"status":"degraded"`) {
		t.Fatalf("healthz = %s", body)
	}
}

func TestConfigChangesAreAudited(t *testing.T) {
	deps, dir := testDeps(t)
	cfg, _ := config.Load(func(string) string { return "" })
	if _, err := New(cfg, slog.New(slog.DiscardHandler), deps); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keys", "tavily.yaml"), []byte("keys:\n  - key: tvly-audit-0001\n    label: audited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps.Conf.Reload()
	rows, err := deps.Store.ListAudit(context.Background(), store.AuditQuery{Action: "config.change"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows = %v, %v", rows, err)
	}
	r := rows[0]
	if r.Source != store.AuditFile || r.Target != "keys/tavily.yaml" || !strings.Contains(r.Diff, "+key tavily audited") || strings.Contains(r.Diff, "tvly-audit-0001") {
		t.Fatalf("audit row = %+v", r)
	}
}

func TestShutdownFlushesKeyState(t *testing.T) {
	deps, dir := testDeps(t)
	if err := os.WriteFile(filepath.Join(dir, "keys", "exa.yaml"), []byte("keys:\n  - exa-shutdown-key-0001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln1, _ := net.Listen("tcp", "127.0.0.1:0")
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	gw, ad := ln1.Addr().String(), ln2.Addr().String()
	ln1.Close()
	ln2.Close()
	cfg, _ := config.Load(func(k string) string {
		return map[string]string{"WARDEN_LISTEN": gw, "WARDEN_ADMIN_LISTEN": ad}[k]
	})
	a, err := New(cfg, slog.New(slog.DiscardHandler), deps)
	if err != nil {
		t.Fatal(err)
	}
	deps.Conf.Reload() // the subscription applies the new key to the pool
	if len(deps.Pool.Views()) != 1 {
		t.Fatalf("pool views = %+v", deps.Pool.Views())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(100 * time.Millisecond) // well under the 5 s flush tick
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	states, err := deps.Store.ListKeyStates(context.Background(), "exa")
	if err != nil || len(states) != 1 || states[0].Status != "active" {
		t.Fatalf("key state not flushed on shutdown: %+v %v", states, err)
	}
}
