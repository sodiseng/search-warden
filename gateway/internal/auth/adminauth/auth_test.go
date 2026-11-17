package adminauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/password"
	"github.com/sodiseng/search-warden/gateway/internal/auth/totp"
	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

const pw = "correct-horse-battery"

var (
	hashOnce sync.Once
	pwHash   string
)

type fixture struct {
	t     *testing.T
	a     *Auth
	st    *store.Store
	cfg   confstore.AdminConfig
	clock time.Time
}

func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()
	hashOnce.Do(func() { pwHash, _ = password.Hash(pw) })
	st, err := store.Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(t.TempDir(), "db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, st: st, clock: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC),
		cfg: confstore.AdminConfig{Username: "admin", PasswordHash: pwHash, SessionIdleMinutes: 60, SessionMaxDays: 7}}
	opts.Now = func() time.Time { return f.clock }
	f.a, err = New(st, func() confstore.AdminConfig { return f.cfg }, opts)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) login(user, pass, code string) (*http.Cookie, error) {
	c, _, err := f.a.Login(context.Background(), "198.51.100.1", "test-agent", user, pass, code)
	return c, err
}

func (f *fixture) req(method, path string, c *http.Cookie) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	if c != nil {
		r.AddCookie(c)
	}
	return r
}

func TestLoginCookieAndHashedStorage(t *testing.T) {
	f := newFixture(t, Options{CookieSecure: true})
	c, err := f.login("admin", pw, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "__Host-warden_admin" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie = %+v", c)
	}
	sessions, _ := f.st.ListSessions(context.Background(), f.clock)
	if len(sessions) != 1 || sessions[0].IDHash == c.Value || !strings.Contains(sessions[0].IDHash, hashID(c.Value)) {
		t.Fatalf("session must be stored as a hash only: %+v", sessions)
	}
	// Sub-path or plain HTTP cannot use the __Host- prefix.
	g := newFixture(t, Options{BasePath: "/admin/"})
	c2, _ := g.login("admin", pw, "")
	if c2.Name != "warden_admin" || c2.Path != "/admin/" || c2.Secure {
		t.Fatalf("cookie = %+v", c2)
	}
}

func TestFailuresAreIndistinguishable(t *testing.T) {
	f := newFixture(t, Options{})
	_, e1 := f.login("admin", "wrong-password-xx", "")
	_, e2 := f.login("root", pw, "")
	if !errors.Is(e1, ErrInvalidCredentials) || !errors.Is(e2, ErrInvalidCredentials) || e1.Error() != e2.Error() {
		t.Fatalf("errors differ: %v / %v", e1, e2)
	}
	audit, _ := f.st.ListAudit(context.Background(), store.AuditQuery{Action: "admin.login_failed"})
	if len(audit) != 2 || audit[0].IP != "198.51.100.1" {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestTOTPAndReplay(t *testing.T) {
	f := newFixture(t, Options{})
	secret, _ := totp.GenerateSecret()
	f.cfg.TOTPSecret = secret
	code, _ := totp.Code(secret, f.clock)
	if _, err := f.login("admin", pw, ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("missing code accepted")
	}
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	if _, err := f.login("admin", pw, wrong); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("wrong code accepted")
	}
	if _, err := f.login("admin", pw, code); err != nil {
		t.Fatalf("valid code rejected: %v", err)
	}
	if _, err := f.login("admin", pw, code); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("replayed code accepted")
	}
}

func TestThrottling(t *testing.T) {
	f := newFixture(t, Options{})
	for i := 0; i < ipMaxFailures; i++ {
		if _, err := f.login("admin", "nope-nope-nope", ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, err := f.login("admin", pw, "") // correct password, but locked out
	var le *LockedError
	if !errors.As(err, &le) || le.RetryAfter <= 0 || le.RetryAfter > lockoutBase {
		t.Fatalf("want lockout, got %v", err)
	}
	f.clock = f.clock.Add(lockoutBase + time.Second)
	if _, err := f.login("admin", pw, ""); err != nil {
		t.Fatalf("after lockout: %v", err)
	}
	// Second lockout doubles.
	for i := 0; i < ipMaxFailures; i++ {
		_, _ = f.login("admin", "nope-nope-nope", "")
	}
	if _, err := f.login("admin", pw, ""); !errors.As(err, &le) || le.RetryAfter <= lockoutBase {
		// A success reset the IP's history, so the second lockout starts at base again.
		if le == nil {
			t.Fatalf("want lockout, got %v", err)
		}
	}
}

func TestGlobalLimitAcrossIPs(t *testing.T) {
	f := newFixture(t, Options{})
	for i := 0; i < globalMax; i++ {
		ip := "203.0.113." + string(rune('0'+i%10)) + string(rune('0'+i/10))
		_, _, _ = f.a.Login(context.Background(), ip, "", "admin", "nope-nope-nope", "")
	}
	_, _, err := f.a.Login(context.Background(), "192.0.2.99", "", "admin", pw, "")
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("global limit not enforced: %v", err)
	}
}

func TestAuthenticateIdleAndAbsoluteExpiry(t *testing.T) {
	f := newFixture(t, Options{})
	c, _ := f.login("admin", pw, "")
	if s, _ := f.a.Authenticate(f.req("GET", "/", c)); s == nil {
		t.Fatal("fresh session rejected")
	}
	f.clock = f.clock.Add(59 * time.Minute)
	if s, _ := f.a.Authenticate(f.req("GET", "/", c)); s == nil {
		t.Fatal("session within idle window rejected")
	}
	f.clock = f.clock.Add(61 * time.Minute) // idle since the touch above
	if s, _ := f.a.Authenticate(f.req("GET", "/", c)); s != nil {
		t.Fatal("idle session accepted")
	}
	// Absolute expiry even when active.
	c, _ = f.login("admin", pw, "")
	deadline := f.clock.Add(7*24*time.Hour + time.Hour)
	for f.clock.Before(deadline) { // stay active: never idle for more than 59 minutes
		f.clock = f.clock.Add(time.Hour - time.Minute)
		f.a.Authenticate(f.req("GET", "/", c))
	}
	if s, _ := f.a.Authenticate(f.req("GET", "/", c)); s != nil {
		t.Fatal("session past absolute expiry accepted")
	}
	if s, _ := f.a.Authenticate(f.req("GET", "/", &http.Cookie{Name: f.a.CookieName(), Value: "forged"})); s != nil {
		t.Fatal("forged cookie accepted")
	}
}

func TestRequireMiddleware(t *testing.T) {
	f := newFixture(t, Options{BasePath: "/admin/"})
	var seen *Session
	h := f.a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	r := f.req("GET", "/keys?x=1", nil)
	r.Header.Set("Accept", "text/html")
	if rec := serve(r); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/login?next=%2Fadmin%2Fkeys%3Fx%3D1" {
		t.Fatalf("browser redirect: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	r = f.req("POST", "/keys", nil)
	r.Header.Set("HX-Request", "true")
	if rec := serve(r); rec.Code != http.StatusUnauthorized || rec.Header().Get("HX-Redirect") != "/admin/login" {
		t.Fatalf("htmx: %d %v", rec.Code, rec.Header())
	}

	c, s, err := f.a.Login(context.Background(), "198.51.100.1", "", "admin", pw, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec := serve(f.req("GET", "/", c)); rec.Code != http.StatusNoContent || seen == nil || seen.CSRF != s.CSRF {
		t.Fatalf("authenticated GET: %d", rec.Code)
	}

	post := func(token, origin, site string) int {
		r := httptest.NewRequest("POST", "/keys", strings.NewReader("csrf="+token))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Host = "panel.example.com"
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		r.AddCookie(c)
		return serve(r).Code
	}
	if code := post("", "https://panel.example.com", ""); code != http.StatusForbidden {
		t.Errorf("missing CSRF: %d", code)
	}
	if code := post("wrong", "https://panel.example.com", ""); code != http.StatusForbidden {
		t.Errorf("wrong CSRF: %d", code)
	}
	if code := post(s.CSRF, "https://evil.example", ""); code != http.StatusForbidden {
		t.Errorf("cross origin: %d", code)
	}
	if code := post(s.CSRF, "https://panel.example.com", "cross-site"); code != http.StatusForbidden {
		t.Errorf("Sec-Fetch-Site cross-site: %d", code)
	}
	if code := post(s.CSRF, "https://panel.example.com", "same-origin"); code != http.StatusNoContent {
		t.Errorf("valid POST: %d", code)
	}
	r = f.req("POST", "/keys", c)
	r.Header.Set("X-CSRF-Token", s.CSRF) // htmx sends the header
	if rec := serve(r); rec.Code != http.StatusNoContent {
		t.Errorf("header CSRF: %d", rec.Code)
	}
}

func TestPublicURLOriginCheck(t *testing.T) {
	f := newFixture(t, Options{PublicURL: "https://admin.example.com"})
	r := httptest.NewRequest("POST", "/", nil)
	r.Host = "warden:8081" // what the reverse proxy forwards
	r.Header.Set("Origin", "https://admin.example.com")
	if !f.a.CheckOrigin(r) {
		t.Error("public URL origin rejected")
	}
	r.Header.Set("Origin", "http://admin.example.com")
	if f.a.CheckOrigin(r) {
		t.Error("scheme mismatch accepted")
	}
}

func TestCredentialChangeRevokesAllSessions(t *testing.T) {
	f := newFixture(t, Options{})
	c1, _ := f.login("admin", pw, "")
	_, _ = f.login("admin", pw, "")
	old := &confstore.Snapshot{Config: confstore.Config{Admin: f.cfg}}
	f.cfg.PasswordHash, _ = password.Hash("a-brand-new-password")
	f.a.HandleConfigChange(confstore.Change{Source: confstore.SourceFile, Old: old, New: &confstore.Snapshot{Config: confstore.Config{Admin: f.cfg}}})
	if s, _ := f.a.Authenticate(f.req("GET", "/", c1)); s != nil {
		t.Fatal("session survived a password change")
	}
	audit, _ := f.st.ListAudit(context.Background(), store.AuditQuery{Action: "admin.sessions_revoked"})
	if len(audit) != 1 || !strings.Contains(audit[0].Diff, "2 session(s)") {
		t.Fatalf("audit = %+v", audit)
	}
	// Unrelated config changes do not log anyone out.
	c3, _ := f.login("admin", "a-brand-new-password", "")
	same := &confstore.Snapshot{Config: confstore.Config{Admin: f.cfg}}
	f.a.HandleConfigChange(confstore.Change{Old: same, New: same})
	if s, _ := f.a.Authenticate(f.req("GET", "/", c3)); s == nil {
		t.Fatal("unrelated change revoked sessions")
	}
}

func TestLogoutRevokeAndCleanup(t *testing.T) {
	f := newFixture(t, Options{})
	c1, s1, _ := f.a.Login(context.Background(), "198.51.100.1", "", "admin", pw, "")
	c2, s2, _ := f.a.Login(context.Background(), "198.51.100.1", "", "admin", pw, "")
	rec := httptest.NewRecorder()
	if err := f.a.Logout(context.Background(), rec, s1, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Error("logout did not clear the cookie")
	}
	if s, _ := f.a.Authenticate(f.req("GET", "/", c1)); s != nil {
		t.Fatal("logged-out session still valid")
	}
	if err := f.a.Revoke(context.Background(), s2.IDHash, ""); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.a.Authenticate(f.req("GET", "/", c2)); s != nil {
		t.Fatal("revoked session still valid")
	}
	_, _, _ = f.a.Login(context.Background(), "198.51.100.1", "", "admin", pw, "")
	f.clock = f.clock.Add(2 * time.Hour)
	f.a.Cleanup(context.Background())
	if left, _ := f.a.Sessions(context.Background()); len(left) != 0 {
		t.Fatalf("idle session not cleaned: %d", len(left))
	}
}

func TestSafeNext(t *testing.T) {
	f := newFixture(t, Options{BasePath: "/admin/"})
	cases := map[string]string{
		"":                        "/admin/",
		"/admin/keys?x=1":         "/admin/keys?x=1",
		"//evil.example/admin/":   "/admin/",
		"https://evil.example/":   "/admin/",
		"/other/":                 "/admin/",
		"/admin/\\evil":           "/admin/",
		"/admin/x\r\nSet-Cookie:": "/admin/",
	}
	for in, want := range cases {
		if got := f.a.SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}
