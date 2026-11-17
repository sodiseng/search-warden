// Package admin serves the admin panel (ARCHITECTURE.md §16): server-rendered
// html/template pages with htmx for partial updates, no inline scripts or
// styles (strict CSP), and every route except the login page and static
// assets behind adminauth.Require (CSRF and Origin checked on every write).
package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/adminauth"
	"github.com/sodiseng/search-warden/gateway/internal/auth/password"
	"github.com/sodiseng/search-warden/gateway/internal/auth/totp"
	"github.com/sodiseng/search-warden/gateway/internal/buildinfo"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/httpx"
	"github.com/sodiseng/search-warden/gateway/internal/limits"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/retention"
	"github.com/sodiseng/search-warden/gateway/internal/store"
	"github.com/sodiseng/search-warden/gateway/web"
)

// Store is the part of *store.Store the panel reads (and the audit log it writes).
type Store interface {
	Totals(ctx context.Context, from, to time.Time) (store.Totals, error)
	Series(ctx context.Context, from, to time.Time, bucket time.Duration) ([]store.SeriesPoint, error)
	Top(ctx context.Context, from, to time.Time, dim string, n int) ([]store.TopRow, error)
	QueryRequestLogs(ctx context.Context, q store.LogQuery) ([]store.RequestLog, error)
	GetRequestLog(ctx context.Context, id int64) (store.RequestLog, error)
	ListAttempts(ctx context.Context, requestID string) ([]store.Attempt, error)
	ListAudit(ctx context.Context, q store.AuditQuery) ([]store.AuditEntry, error)
	AppendAudit(ctx context.Context, e store.AuditEntry) (int64, error)
}

// Options wires the panel. Everything but Auth and Conf is optional; a page
// whose source is missing shows no data.
type Options struct {
	Auth        *adminauth.Auth
	Conf        *confstore.Manager
	Store       Store
	BasePath    string // "/" or e.g. "/admin/"
	GatewayURL  string // WARDEN_PUBLIC_URL, shown in the client-config hints ("" = a placeholder)
	EnvPassword bool   // WARDEN_ADMIN_PASSWORD is still set (shown as a hint)
	Keys        func() []pool.View
	Egresses    func() []outbound.View
	ProbeEgress func(ids ...string)               // test egresses now (all when none given)
	SyncQuota   func(ids ...string) int           // queue quota syncs (all when none given); nil hides the buttons
	TestKeys    func(ids ...string) int           // test keys (quota sync or free probe); nil hides the button
	TokenUsage  func(tokenID string) limits.Usage // current day/month counters of a token
	LogStatus   func() retention.Status           // the last log-retention pass
	Now         func() time.Time
	Logger      *slog.Logger
}

// Panel is the admin http.Handler; mount it with http.StripPrefix(BasePath).
type Panel struct {
	auth     *adminauth.Auth
	conf     *confstore.Manager
	st       Store
	base     string
	gwURL    string
	envPw    bool
	keys     func() []pool.View
	egresses func() []outbound.View
	probe    func(ids ...string)
	sync     func(ids ...string) int
	usage    func(tokenID string) limits.Usage
	test     func(ids ...string) int
	logStat  func() retention.Status
	now      func() time.Time
	log      *slog.Logger
	pages    map[lang]map[string]*template.Template // one parsed set per language
	routes   []route
	mux      *http.ServeMux
}

// route is one panel endpoint. Public routes are the explicit exception to
// "everything requires a session"; TestRouteTable pins the list.
type route struct {
	pattern string
	public  bool
	h       http.HandlerFunc
}

var pageNames = []string{"login", "home", "security", "totp_setup", "requests", "request", "keys", "key",
	"egresses", "egress", "tokens", "token", "secret", "settings", "files", "file"}

// New parses templates and builds the routing table.
func New(o Options) (*Panel, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Keys == nil {
		o.Keys = func() []pool.View { return nil }
	}
	if o.Egresses == nil {
		o.Egresses = func() []outbound.View { return nil }
	}
	if o.ProbeEgress == nil {
		o.ProbeEgress = func(...string) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TokenUsage == nil {
		o.TokenUsage = func(string) limits.Usage { return limits.Usage{} }
	}
	if o.LogStatus == nil {
		o.LogStatus = func() retention.Status { return retention.Status{} }
	}
	p := &Panel{test: o.TestKeys, usage: o.TokenUsage, logStat: o.LogStatus, auth: o.Auth, conf: o.Conf, st: o.Store, base: o.BasePath, gwURL: strings.TrimRight(o.GatewayURL, "/"), envPw: o.EnvPassword, keys: o.Keys, egresses: o.Egresses,
		probe: o.ProbeEgress, sync: o.SyncQuota, now: o.Now, log: o.Logger, pages: map[lang]map[string]*template.Template{}}
	// The helpers that produce text are bound to a language, so the templates are
	// parsed once per language and each request picks its set.
	for _, l := range langs {
		fm := funcs(o.Now, l, o.BasePath)
		p.pages[l] = map[string]*template.Template{}
		for _, name := range pageNames {
			t, err := template.New("").Funcs(fm).ParseFS(web.FS, "templates/layout.html", "templates/partials.html", "templates/"+name+".html")
			if err != nil {
				return nil, fmt.Errorf("admin template %s: %w", name, err)
			}
			p.pages[l][name] = t
		}
	}
	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, err
	}
	etags, err := staticETags(static)
	if err != nil {
		return nil, err
	}
	files := http.FileServerFS(static)
	p.routes = []route{
		{"GET /static/", true, func(w http.ResponseWriter, r *http.Request) {
			// Revalidate every time: the ?v= query only changes when the version
			// string does, and a local build keeps it at "dev" (found in use
			// 2026-11-12: a stale app.css hid a layout fix for an hour). The ETag
			// turns that revalidation into a 304 instead of a fresh download.
			name := strings.TrimPrefix(r.URL.Path, "/static/")
			if tag, ok := etags[name]; ok {
				w.Header().Set("ETag", tag)
			}
			if ct, ok := staticTypes[path.Ext(name)]; ok {
				w.Header().Set("Content-Type", ct)
			}
			w.Header().Set("Cache-Control", "no-cache")
			http.StripPrefix("/static", files).ServeHTTP(w, r)
		}},
		{"GET /login", true, p.loginPage},
		{"POST /login", true, p.loginSubmit},
		{"POST /logout", false, p.logout},
		{"GET /{$}", false, p.home},
		{"GET /requests", false, p.requests},
		{"GET /requests/rows", false, p.requestRows},
		{"GET /requests/{id}", false, p.requestDetail},
		{"GET /keys", false, p.keysPage},
		{"GET /keys/new", false, p.keyNew},
		{"POST /keys/add", false, p.keysAdd},
		{"POST /keys/batch", false, p.keysBatch},
		{"GET /keys/{id}", false, p.keyPage},
		{"POST /keys/{id}", false, p.keySave},
		{"POST /keys/{id}/reveal", false, p.keyReveal},
		{"GET /egresses", false, p.egressesPage},
		{"GET /egresses/import", false, p.egressImportDialog},
		{"POST /egresses/import", false, p.egressImport},
		{"POST /egresses/test", false, p.egressTest},
		{"POST /egresses/export", false, p.egressExport},
		{"GET /egresses/{name}", false, p.egressPage},
		{"POST /egresses/{name}", false, p.egressSave},
		{"POST /egresses/{name}/rename", false, p.egressRename},
		{"POST /egresses/{name}/delete", false, p.egressDelete},
		{"GET /tokens", false, p.tokensPage},
		{"GET /tokens/new", false, p.tokenNew},
		{"POST /tokens/add", false, p.tokenAdd},
		{"GET /tokens/{id}", false, p.tokenPage},
		{"POST /tokens/{id}", false, p.tokenSave},
		{"POST /tokens/{id}/rotate", false, p.tokenRotate},
		{"POST /tokens/{id}/toggle", false, p.tokenToggle},
		{"POST /tokens/{id}/delete", false, p.tokenDelete},
		{"GET /settings", false, p.settingsPage},
		{"POST /settings/{group}", false, p.settingsSave},
		{"GET /files", false, p.filesPage},
		{"GET /files/edit", false, p.filePage},
		{"POST /files/edit", false, p.fileSave},
		{"POST /files/export", false, p.filesExport},
		{"GET /security", false, p.security},
		{"POST /security/sessions/revoke", false, p.revokeSession},
		{"POST /security/sessions/revoke-others", false, p.revokeOthers},
		{"POST /security/password", false, p.changePassword},
		{"GET /security/totp/setup", false, p.totpSetup},
		{"POST /security/totp/enable", false, p.totpEnable},
		{"POST /security/totp/disable", false, p.totpDisable},
		{"POST /quota/sync", false, p.syncQuota},
		{"/", false, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }}, // deny by default
	}
	p.mux = http.NewServeMux()
	for _, rt := range p.routes {
		var h http.Handler = rt.h
		if !rt.public {
			h = p.auth.Require(h)
		}
		p.mux.Handle(rt.pattern, h)
	}
	return p, nil
}

func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.mux.ServeHTTP(w, r) }

// page is the data for every template.
type page struct {
	Title, Nav, Flash, Error string
	Base, CSRF, Version      string
	LoggedIn                 bool
	// Lang ("zh" / "en") and Theme ("light" / "dark") come from the display
	// cookies; HTMLLang is the <html lang> value. Self is where the language
	// switch reloads to when this page answered a POST (reloading that would
	// submit the form again); it is empty for a page that answered a GET.
	Lang, HTMLLang, Theme, Self string
	// FlashInCard renders the flash/error inside the card instead of above it.
	// The standalone pages (login, TOTP setup) use it: a message above the card
	// used to shift the card, and next to the form it cannot be missed.
	FlashInCard bool

	Next string // login
	TOTP bool

	Sessions    []sessionRow // security
	TOTPEnabled bool
	MinPassword int

	Secret, URI string // totp setup

	Data any // the page's own data
}

type sessionRow struct {
	ID, IP, UA, UAShort, Created, LastSeen string
	Current                                bool
}

type auditData struct {
	Source  string
	Entries []store.AuditEntry
}

// flashes are fixed messages selected by ?ok=…, so no user input is ever echoed.
var flashes = map[string]msg{
	"password":       {"密码已修改，其他会话已全部退出。", "Password changed; every other session was signed out."},
	"totp_on":        {"两步验证已开启，其他会话已全部退出。", "Two-step verification is on; every other session was signed out."},
	"totp_off":       {"两步验证已关闭，其他会话已全部退出。", "Two-step verification is off; every other session was signed out."},
	"revoked":        {"已退出该会话。", "That session was signed out."},
	"revoked_others": {"已退出其他所有会话。", "Every other session was signed out."},
	"sync": {"已加入同步队列。为遵守 Tavily 的限速，每个 key、每个出口 IP 至少间隔 65 秒：同一出口上的 key 依次同步，每个约 1 分钟。",
		"Queued. To respect Tavily's rate limit, syncs are at least 65 seconds apart per key and per exit IP: keys behind one egress sync one after another, about a minute each."},
	"sync_none": {"没有可以同步的 key（Exa 没有余额接口，只按实际费用本地记账）。",
		"No key can be synced (Exa has no balance API; its spend is tracked locally from actual costs)."},
	"saved":        {"已保存并生效。", "Saved and applied."},
	"deleted":      {"已删除。", "Deleted."},
	"keys_added":   {"key 已添加。", "Keys added."},
	"keys_updated": {"所选 key 已更新。", "The selected keys were updated."},
	"keys_deleted": {"所选 key 已删除。", "The selected keys were deleted."},
	"testing":      {"已开始测试，几秒后刷新页面查看结果。", "Testing started; reload in a few seconds to see the result."},
	"testing_keys": {"已开始测试：Exa key 几秒内出结果；Tavily key 通过同步额度来测试，同一出口上的 key 每 65 秒测一个。刷新页面查看结果。",
		"Testing started: Exa keys report within seconds; Tavily keys are tested by a quota sync, one key per 65 seconds behind the same egress. Reload to see the result."},
	"imported":  {"节点已导入，正在测试连通性。", "Nodes imported; testing connectivity."},
	"renamed":   {"已重命名，引用它的 key 已同步修改。", "Renamed; the keys that reference it were updated."},
	"token_on":  {"令牌已启用。", "Token enabled."},
	"token_off": {"令牌已停用。", "Token disabled."},
}

// navRoot is each section's list page: where a page that answered a POST sends
// the language switch (page.Self).
var navRoot = map[string]string{"home": "", "requests": "requests", "keys": "keys", "egresses": "egresses",
	"tokens": "tokens", "settings": "settings", "files": "files", "security": "security"}

// fill sets what every rendered page or fragment needs and returns its language.
func (p *Panel) fill(r *http.Request, d *page) lang {
	l := langOf(r)
	d.Base, d.Version = p.base, buildinfo.Version
	d.Lang, d.HTMLLang, d.Theme = string(l), l.html(), themeOf(r)
	if s := adminauth.FromContext(r.Context()); s != nil {
		d.LoggedIn, d.CSRF = true, s.CSRF
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		d.Self = p.base + navRoot[d.Nav]
		if !d.LoggedIn {
			d.Self = p.base + "login"
		}
	}
	return l
}

func (p *Panel) render(w http.ResponseWriter, r *http.Request, status int, name string, d page) {
	l := p.fill(r, &d)
	if d.Flash == "" {
		d.Flash = flashes[r.URL.Query().Get("ok")].in(l)
	}
	p.execute(w, status, name, "layout", d)
}

// htmx reports its own requests, so a handler can answer with a fragment (the body of
// the shared dialog, or a table body) instead of a whole page.
func htmx(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// modal renders a page's "modal" block: the body of the shared <dialog>. Every
// create / edit flow is a modal, so nothing navigates away to a form page.
func (p *Panel) modal(w http.ResponseWriter, r *http.Request, status int, name string, d page) {
	p.fill(r, &d)
	p.execute(w, status, name, "modal", d)
}

// dialog answers a dialog flow: the fragment for the shared <dialog> when htmx asked
// for it, the page's full-page fallback otherwise (a direct visit to the address).
func (p *Panel) dialog(w http.ResponseWriter, r *http.Request, status int, name string, d page) {
	if htmx(r) {
		p.modal(w, r, status, name, d)
		return
	}
	p.render(w, r, status, name, d)
}

// done finishes a modal form: an htmx request is told to navigate (which closes the
// dialog and lands on the list page), a plain browser request gets a normal redirect.
func (p *Panel) done(w http.ResponseWriter, r *http.Request, path string) {
	if htmx(r) {
		w.Header().Set("HX-Redirect", p.base+strings.TrimPrefix(path, "/"))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p.redirect(w, r, path)
}

// renderPart renders one named block of a page (htmx partial updates).
func (p *Panel) renderPart(w http.ResponseWriter, r *http.Request, name, block string, d page) {
	p.fill(r, &d)
	p.execute(w, http.StatusOK, name, block, d)
}

func (p *Panel) execute(w http.ResponseWriter, status int, name, block string, d page) {
	var buf bytes.Buffer
	if err := p.pages[lang(d.Lang)][name].ExecuteTemplate(&buf, block, d); err != nil {
		p.log.Error("render failed", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// staticTypes pins the Content-Type of the embedded assets. Go otherwise asks the
// operating system (the Windows registry, /etc/mime.types), which a distroless
// image does not have, and the panel sends nosniff.
var staticTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".woff2": "font/woff2",
}

// staticETags hashes every embedded asset once. Embedded files carry no
// modification time, so without an ETag "Cache-Control: no-cache" would mean a
// full download of every stylesheet, script and font on every page view.
func staticETags(static fs.FS) (map[string]string, error) {
	tags := map[string]string{}
	err := fs.WalkDir(static, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(static, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		tags[name] = `"` + hex.EncodeToString(sum[:12]) + `"`
		return nil
	})
	return tags, err
}

func (p *Panel) redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, p.base+strings.TrimPrefix(path, "/"), http.StatusSeeOther)
}

// audit records a panel action that changes no config file (those are logged
// by the config subscription), e.g. revealing a key or exporting the config.
func (p *Panel) audit(r *http.Request, action, target string) {
	if p.st == nil {
		return
	}
	if _, err := p.st.AppendAudit(r.Context(), store.AuditEntry{Source: store.AuditPanel, IP: clientIP(r), Action: action, Target: target}); err != nil {
		p.log.Warn("audit write failed", "err", err)
	}
}

// saveError turns a config edit error into a message for the page. The error
// text itself comes from confstore and is English.
func saveError(l lang, err error) string {
	var conflict *confstore.ConflictError
	if errors.As(err, &conflict) {
		return l.tr("保存失败：文件刚被其他人或手工修改过，请刷新页面后重试（", "Not saved: the file was just changed by someone else or by hand; reload the page and try again (") +
			err.Error() + l.tr("）", ")")
	}
	return l.tr("保存失败：", "Not saved: ") + err.Error()
}

func clientIP(r *http.Request) string { return httpx.ClientIPFrom(r.Context()).String() }

// ---- login --------------------------------------------------------------

func (p *Panel) loginPage(w http.ResponseWriter, r *http.Request) {
	if s, _ := p.auth.Authenticate(r); s != nil {
		http.Redirect(w, r, p.auth.SafeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	p.render(w, r, http.StatusOK, "login", page{Title: langOf(r).tr("登录", "Sign in"), Next: p.auth.SafeNext(r.URL.Query().Get("next")), TOTP: p.auth.TOTPEnabled(), FlashInCard: true})
}

func (p *Panel) loginSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	l := langOf(r)
	d := page{Title: l.tr("登录", "Sign in"), Next: p.auth.SafeNext(r.PostFormValue("next")), TOTP: p.auth.TOTPEnabled(), FlashInCard: true}
	if !p.auth.CheckOrigin(r) {
		p.log.Warn("login refused: cross-origin", "detail", p.auth.OriginDetail(r))
		d.Error = l.tr("请求来源不正确（", "The request did not come from this panel (") + p.auth.OriginDetail(r) +
			l.tr("）。经反向代理访问时，把 WARDEN_ADMIN_PUBLIC_URL 设为外部访问地址；只在本机访问时留空。",
				"). Behind a reverse proxy, set WARDEN_ADMIN_PUBLIC_URL to the external address; leave it empty when the panel is only opened locally.")
		p.render(w, r, http.StatusForbidden, "login", d)
		return
	}
	c, _, err := p.auth.Login(r.Context(), clientIP(r), r.UserAgent(),
		r.PostFormValue("username"), r.PostFormValue("password"), r.PostFormValue("totp"))
	var locked *adminauth.LockedError
	switch {
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", fmt.Sprint(int(locked.RetryAfter.Seconds())+1))
		d.Error = tooManyAttempts(l, locked.RetryAfter)
		p.render(w, r, http.StatusTooManyRequests, "login", d)
		return
	case errors.Is(err, adminauth.ErrInvalidCredentials):
		d.Error = l.tr("用户名、密码或验证码错误。", "Wrong username, password or code.")
		p.render(w, r, http.StatusUnauthorized, "login", d)
		return
	case err != nil:
		p.log.Error("login failed", "err", err)
		d.Error = l.tr("登录失败，请查看服务日志。", "Sign-in failed; see the service log.")
		p.render(w, r, http.StatusInternalServerError, "login", d)
		return
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, d.Next, http.StatusSeeOther)
}

func tooManyAttempts(l lang, wait time.Duration) string {
	return fmt.Sprintf(l.tr("尝试次数过多，请 %s 后再试。", "Too many attempts; try again in %s."), humanDuration(l, wait))
}

func (p *Panel) logout(w http.ResponseWriter, r *http.Request) {
	_ = p.auth.Logout(r.Context(), w, adminauth.FromContext(r.Context()), clientIP(r))
	p.redirect(w, r, "login")
}

// syncQuota queues a quota sync of every syncable key; the gates decide when each runs.

func (p *Panel) security(w http.ResponseWriter, r *http.Request) {
	p.renderSecurity(w, r, http.StatusOK, "")
}

func (p *Panel) renderSecurity(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	cur := adminauth.FromContext(r.Context())
	list, err := p.auth.Sessions(r.Context())
	if err != nil {
		p.log.Error("list sessions", "err", err)
	}
	rows := make([]sessionRow, 0, len(list))
	for _, s := range list {
		rows = append(rows, sessionRow{ID: s.IDHash, IP: s.IP, UA: s.UserAgent, UAShort: shortUA(s.UserAgent),
			Created: s.CreatedAt.Local().Format("2006-01-02 15:04"), LastSeen: s.LastSeenAt.Local().Format("2006-01-02 15:04"),
			Current: cur != nil && s.IDHash == cur.IDHash})
	}
	src := r.URL.Query().Get("source")
	if src != store.AuditPanel && src != store.AuditFile && src != store.AuditSystem {
		src = ""
	}
	var audit []store.AuditEntry
	if p.st != nil {
		audit, err = p.st.ListAudit(r.Context(), store.AuditQuery{Source: src, Limit: 100})
		if err != nil {
			p.log.Error("list audit", "err", err)
		}
	}
	p.render(w, r, status, "security", page{Title: langOf(r).tr("安全", "Security"), Nav: "security", Error: errMsg, Sessions: rows,
		TOTPEnabled: p.auth.TOTPEnabled(), MinPassword: password.MinLength, Data: auditData{Source: src, Entries: audit}})
}

func (p *Panel) revokeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if cur := adminauth.FromContext(r.Context()); cur != nil && id == cur.IDHash {
		p.renderSecurity(w, r, http.StatusBadRequest, langOf(r).tr("不能在这里退出当前会话，请使用右上角的\"退出登录\"。",
			"This is your current session; use \"Sign out\" in the top bar instead."))
		return
	}
	if err := p.auth.Revoke(r.Context(), id, clientIP(r)); err != nil {
		p.log.Error("revoke session", "err", err)
	}
	p.redirect(w, r, "security?ok=revoked")
}

func (p *Panel) revokeOthers(w http.ResponseWriter, r *http.Request) {
	cur := adminauth.FromContext(r.Context())
	list, err := p.auth.Sessions(r.Context())
	if err != nil {
		p.log.Error("list sessions", "err", err)
	}
	for _, s := range list {
		if s.IDHash != cur.IDHash {
			_ = p.auth.Revoke(r.Context(), s.IDHash, clientIP(r))
		}
	}
	p.redirect(w, r, "security?ok=revoked_others")
}

// reauth re-verifies the current password; it renders the error itself.
func (p *Panel) reauth(w http.ResponseWriter, r *http.Request) bool {
	err := p.auth.CheckPassword(r.Context(), clientIP(r), r.PostFormValue("current"))
	var locked *adminauth.LockedError
	switch {
	case err == nil:
		return true
	case errors.As(err, &locked):
		p.renderSecurity(w, r, http.StatusTooManyRequests, tooManyAttempts(langOf(r), locked.RetryAfter))
	default:
		p.renderSecurity(w, r, http.StatusUnauthorized, wrongPassword(langOf(r)))
	}
	return false
}

func wrongPassword(l lang) string {
	return l.tr("当前密码不正确。", "The current password is wrong.")
}

// afterCredentialChange: the change revoked every session (adminauth listens to
// confstore); keep the current admin logged in with a fresh session.
func (p *Panel) afterCredentialChange(w http.ResponseWriter, r *http.Request, flash string) {
	c, _, err := p.auth.NewSession(r.Context(), clientIP(r), r.UserAgent())
	if err != nil {
		p.log.Error("new session after credential change", "err", err)
		p.done(w, r, "login")
		return
	}
	http.SetCookie(w, c)
	p.done(w, r, "security?ok="+flash) // the TOTP dialog posts through htmx
}

func (p *Panel) changePassword(w http.ResponseWriter, r *http.Request) {
	l := langOf(r)
	newPw, confirm := r.PostFormValue("new"), r.PostFormValue("confirm")
	if newPw != confirm {
		p.renderSecurity(w, r, http.StatusBadRequest, l.tr("两次输入的新密码不一致。", "The two new passwords do not match."))
		return
	}
	if err := password.Validate(newPw); err != nil {
		p.renderSecurity(w, r, http.StatusBadRequest, fmt.Sprintf(l.tr("新密码至少需要 %d 个字符。", "The new password needs at least %d characters."), password.MinLength))
		return
	}
	if !p.reauth(w, r) {
		return
	}
	if _, err := p.conf.SetAdminPassword(newPw); err != nil {
		p.log.Error("set admin password", "err", err)
		p.renderSecurity(w, r, http.StatusInternalServerError, saveError(l, err))
		return
	}
	p.afterCredentialChange(w, r, "password")
}

func (p *Panel) totpSetup(w http.ResponseWriter, r *http.Request) {
	if p.auth.TOTPEnabled() {
		p.redirect(w, r, "security")
		return
	}
	secret, err := totp.GenerateSecret()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	user := p.conf.Snapshot().Config.Admin.Username
	pg := page{Title: totpTitle(langOf(r)), Nav: "security", FlashInCard: true,
		Secret: secret, URI: totp.URI(secret, user, "search-warden")}
	p.dialog(w, r, http.StatusOK, "totp_setup", pg)
}

func totpTitle(l lang) string { return l.tr("开启两步验证", "Turn on two-step verification") }

func (p *Panel) totpEnable(w http.ResponseWriter, r *http.Request) {
	l := langOf(r)
	secret := strings.TrimSpace(r.PostFormValue("secret"))
	renderSetup := func(status int, msg string) {
		user := p.conf.Snapshot().Config.Admin.Username
		pg := page{Title: totpTitle(l), Nav: "security", Error: msg, FlashInCard: true,
			Secret: secret, URI: totp.URI(secret, user, "search-warden")}
		p.dialog(w, r, status, "totp_setup", pg)
	}
	if _, ok := totp.Verify(secret, r.PostFormValue("code"), time.Now(), 0); !ok {
		renderSetup(http.StatusBadRequest, l.tr("验证码不正确，请确认手机时间准确，并输入最新的验证码。",
			"Wrong code. Check that the phone's clock is right and enter the newest code."))
		return
	}
	if err := p.auth.CheckPassword(r.Context(), clientIP(r), r.PostFormValue("current")); err != nil {
		renderSetup(http.StatusUnauthorized, wrongPassword(l))
		return
	}
	if _, err := p.conf.SetTOTPSecret(secret); err != nil {
		renderSetup(http.StatusBadRequest, saveError(l, err))
		return
	}
	p.auth.CheckTOTP(r.PostFormValue("code")) // burn the confirmation code so it cannot be replayed at login
	p.afterCredentialChange(w, r, "totp_on")
}

func (p *Panel) totpDisable(w http.ResponseWriter, r *http.Request) {
	if !p.reauth(w, r) {
		return
	}
	if !p.auth.CheckTOTP(r.PostFormValue("code")) {
		p.renderSecurity(w, r, http.StatusUnauthorized, langOf(r).tr("验证码不正确。", "Wrong code."))
		return
	}
	if _, err := p.conf.SetTOTPSecret(""); err != nil {
		p.renderSecurity(w, r, http.StatusInternalServerError, saveError(langOf(r), err))
		return
	}
	p.afterCredentialChange(w, r, "totp_off")
}

func shortUA(ua string) string {
	for _, b := range []string{"Edg/", "Firefox/", "Chrome/", "Safari/", "curl/"} {
		if i := strings.Index(ua, b); i >= 0 {
			name := strings.TrimSuffix(b, "/")
			if name == "Edg" {
				name = "Edge"
			}
			ver := ua[i+len(b):]
			if j := strings.IndexAny(ver, ". "); j > 0 {
				ver = ver[:j]
			}
			return name + " " + ver
		}
	}
	if len(ua) > 40 {
		return ua[:40] + "…"
	}
	return ua
}
