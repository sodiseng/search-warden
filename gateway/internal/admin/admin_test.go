package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/adminauth"
	"github.com/sodiseng/search-warden/gateway/internal/auth/totp"
	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/httpx"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

const pw = "correct-horse-battery"

type env struct {
	t    *testing.T
	srv  *httptest.Server
	conf *confstore.Manager
	st   *store.Store
	dir  string
	p    *Panel
}

func newEnv(t *testing.T, opts ...func(*Options)) *env {
	t.Helper()
	dir := t.TempDir()
	conf, err := confstore.Open(dir, confstore.Options{AdminPasswordEnv: pw})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(dir, "db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	auth, err := adminauth.New(st, func() confstore.AdminConfig { return conf.Snapshot().Config.Admin }, adminauth.Options{BasePath: "/"})
	if err != nil {
		t.Fatal(err)
	}
	conf.Subscribe(auth.HandleConfigChange)
	o := Options{Auth: auth, Conf: conf, Store: st, BasePath: "/"}
	for _, f := range opts {
		f(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	res, _ := httpx.NewClientIPResolver(httpx.DefaultTrustedProxies(), httpx.DefaultClientIPHeaders())
	srv := httptest.NewServer(httpx.Chain(p, httpx.ResolveClientIP(func() *httpx.ClientIPResolver { return res })))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, conf: conf, st: st, dir: dir, p: p}
}

// browser is a cookie-keeping client that does not follow redirects.
func (e *env) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) get(c *http.Client, path string) (int, string, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	req.Header.Set("Accept", "text/html")
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// htmxPost sends a request as the panel's dialog forms do (HX-Request), so the
// handler answers with the dialog fragment.
func (e *env) htmxPost(c *http.Client, path string, form url.Values) (int, string, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	req.Header.Set("HX-Request", "true")
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (e *env) htmxGet(c *http.Client, path string) (int, string, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Accept", "text/html")
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (e *env) post(c *http.Client, path string, form url.Values) (int, string, http.Header) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.srv.URL)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (e *env) csrf(c *http.Client, page string) string {
	e.t.Helper()
	_, body, _ := e.get(c, page)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		e.t.Fatalf("no CSRF token on %s:\n%s", page, body)
	}
	return m[1]
}

func (e *env) login(c *http.Client, password, code string) int {
	e.t.Helper()
	code0, _, _ := e.post(c, "/login", url.Values{"username": {"admin"}, "password": {password}, "totp": {code}, "next": {"/security"}})
	return code0
}

// TestRouteTable pins which routes are reachable without a session (deny by default).
func TestRouteTable(t *testing.T) {
	e := newEnv(t)
	var public []string
	for _, r := range e.p.routes {
		if r.public {
			public = append(public, r.pattern)
		}
	}
	want := []string{"GET /static/", "GET /login", "POST /login"}
	if !slices.Equal(public, want) {
		t.Fatalf("public routes = %v, want %v", public, want)
	}
	anon := e.browser()
	for _, r := range e.p.routes {
		if r.public {
			continue
		}
		method, path, _ := strings.Cut(r.pattern, " ")
		if path == "" { // pattern without method
			method, path = http.MethodGet, method
		}
		path = strings.ReplaceAll(path, "{$}", "")
		var code int
		if method == http.MethodPost {
			code, _, _ = e.post(anon, path, url.Values{})
		} else {
			code, _, _ = e.get(anon, path)
		}
		if code != http.StatusSeeOther && code != http.StatusUnauthorized {
			t.Errorf("%s reachable without a session: %d", r.pattern, code)
		}
	}
}

func TestLoginLogoutFlow(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	code, _, hdr := e.get(b, "/security")
	if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login?next=") {
		t.Fatalf("anonymous page: %d %s", code, hdr.Get("Location"))
	}
	code, body, _ := e.get(b, "/login")
	if code != http.StatusOK || !strings.Contains(body, "登录管理面板") || strings.Contains(body, `name="totp"`) {
		t.Fatalf("login page: %d", code)
	}
	if code := e.login(b, "wrong-password-123", ""); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	code, _, hdr = e.post(b, "/login", url.Values{"username": {"admin"}, "password": {pw}, "next": {"//evil.example/"}})
	if code != http.StatusSeeOther || hdr.Get("Location") != "/" {
		t.Fatalf("login: %d → %s (open redirect must be blocked)", code, hdr.Get("Location"))
	}
	code, body, _ = e.get(b, "/")
	if code != http.StatusOK || !strings.Contains(body, "概览") || !strings.Contains(body, "config.yaml") {
		t.Fatalf("home: %d", code)
	}
	tok := e.csrf(b, "/")
	if code, _, _ := e.post(b, "/logout", url.Values{}); code != http.StatusForbidden {
		t.Fatalf("logout without CSRF must fail: %d", code)
	}
	if code, _, _ := e.post(b, "/logout", url.Values{"csrf": {tok}}); code != http.StatusSeeOther {
		t.Fatalf("logout: %d", code)
	}
	if code, _, _ := e.get(b, "/"); code != http.StatusSeeOther {
		t.Fatalf("after logout: %d", code)
	}
}

func TestCrossOriginLoginRejected(t *testing.T) {
	e := newEnv(t)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/login", strings.NewReader(url.Values{"username": {"admin"}, "password": {pw}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := e.browser().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(resp.Cookies()) != 0 {
		t.Fatalf("cross-origin login: %d, cookies %v", resp.StatusCode, resp.Cookies())
	}
}

func TestChangePasswordKeepsCurrentSessionOnly(t *testing.T) {
	e := newEnv(t)
	me, other := e.browser(), e.browser()
	if e.login(me, pw, "") != http.StatusSeeOther || e.login(other, pw, "") != http.StatusSeeOther {
		t.Fatal("login")
	}
	tok := e.csrf(me, "/security")
	newPw := "the-new-password-456"
	code, body, _ := e.post(me, "/security/password", url.Values{"csrf": {tok}, "current": {"wrong-current-pw"}, "new": {newPw}, "confirm": {newPw}})
	if code != http.StatusUnauthorized || !strings.Contains(body, "当前密码不正确") {
		t.Fatalf("wrong current password: %d", code)
	}
	code, _, hdr := e.post(me, "/security/password", url.Values{"csrf": {tok}, "current": {pw}, "new": {newPw}, "confirm": {newPw}})
	if code != http.StatusSeeOther || hdr.Get("Location") != "/security?ok=password" {
		t.Fatalf("change password: %d %s", code, hdr.Get("Location"))
	}
	if code, body, _ := e.get(me, "/security?ok=password"); code != http.StatusOK || !strings.Contains(body, "密码已修改") {
		t.Fatalf("current session should stay logged in: %d", code)
	}
	if code, _, _ := e.get(other, "/"); code != http.StatusSeeOther {
		t.Fatalf("other session should be revoked: %d", code)
	}
	cfg, _ := os.ReadFile(filepath.Join(e.dir, "config.yaml"))
	if strings.Contains(string(cfg), newPw) || !strings.Contains(string(cfg), "password_hash: $argon2id$") {
		t.Fatal("config.yaml not updated safely")
	}
	if e.login(e.browser(), newPw, "") != http.StatusSeeOther {
		t.Fatal("new password does not work")
	}
}

var secretRe = regexp.MustCompile(`name="secret" value="([A-Z2-7]+)"`)

func TestEnableAndDisableTOTP(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	e.login(b, pw, "")
	_, body, _ := e.get(b, "/security/totp/setup")
	m := secretRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no secret on setup page:\n%s", body)
	}
	secret := m[1]
	tok := csrfRe.FindStringSubmatch(body)[1]
	code, _ := totp.Code(secret, time.Now())
	if c, _, _ := e.post(b, "/security/totp/enable", url.Values{"csrf": {tok}, "secret": {secret}, "code": {"000000"}, "current": {pw}}); c != http.StatusBadRequest && code != "000000" {
		t.Fatalf("wrong code accepted: %d", c)
	}
	if c, _, hdr := e.post(b, "/security/totp/enable", url.Values{"csrf": {tok}, "secret": {secret}, "code": {code}, "current": {pw}}); c != http.StatusSeeOther || !strings.Contains(hdr.Get("Location"), "totp_on") {
		t.Fatalf("enable: %d %s", c, hdr.Get("Location"))
	}
	if e.conf.Snapshot().Config.Admin.TOTPSecret != secret {
		t.Fatal("secret not saved to config.yaml")
	}
	// Login now requires the code.
	fresh := e.browser()
	if c := e.login(fresh, pw, ""); c != http.StatusUnauthorized {
		t.Fatalf("login without code: %d", c)
	}
	_, loginPage, _ := e.get(fresh, "/login")
	if !strings.Contains(loginPage, `name="totp"`) {
		t.Fatal("login page should ask for the code")
	}
	// Disable with password + a fresh code (the enable code was consumed: replay protection).
	next, _ := totp.Code(secret, time.Now().Add(30*time.Second))
	tok = e.csrf(b, "/security")
	if c, _, hdr := e.post(b, "/security/totp/disable", url.Values{"csrf": {tok}, "current": {pw}, "code": {next}}); c != http.StatusSeeOther || !strings.Contains(hdr.Get("Location"), "totp_off") {
		t.Fatalf("disable: %d %s", c, hdr.Get("Location"))
	}
	if e.conf.Snapshot().Config.Admin.TOTPSecret != "" {
		t.Fatal("secret not removed")
	}
}

func TestStaticAndNotFound(t *testing.T) {
	e := newEnv(t)
	code, body, hdr := e.get(e.browser(), "/static/app.css")
	if code != http.StatusOK || !strings.Contains(body, "--accent") || !strings.Contains(hdr.Get("Content-Type"), "text/css") {
		t.Fatalf("static: %d %s", code, hdr.Get("Content-Type"))
	}
	b := e.browser()
	e.login(b, pw, "")
	if code, _, _ := e.get(b, "/no-such-page"); code != http.StatusNotFound {
		t.Fatalf("unknown page for a logged-in admin: %d", code)
	}
}
