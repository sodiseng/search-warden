package admin

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/totp"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/store"
	"github.com/sodiseng/search-warden/gateway/web"
)

// cjk matches Chinese text: Han characters and the full-width punctuation that goes with them.
var cjk = regexp.MustCompile(`[\p{Han}，。、：；（）]`)

func TestLangAndThemeFollowTheCookies(t *testing.T) {
	for _, c := range []struct {
		cookie, accept string
		want           lang
	}{
		{"", "", langZH},
		{"en", "", langEN},
		{"zh", "en-US,en;q=0.9", langZH}, // the switch wins over the browser
		{"fr", "", langZH},               // only the fixed values are accepted
		{`en"><script>`, "", langZH},
		{"", "en-US,en;q=0.9,zh;q=0.8", langEN},
		{"", "fr-FR, zh-TW;q=0.8, en;q=0.5", langZH}, // the first supported one
		{"", "ja", langZH},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.cookie != "" {
			r.AddCookie(&http.Cookie{Name: langCookie, Value: url.QueryEscape(c.cookie)})
		}
		r.Header.Set("Accept-Language", c.accept)
		if got := langOf(r); got != c.want {
			t.Errorf("langOf(cookie %q, Accept-Language %q) = %q, want %q", c.cookie, c.accept, got, c.want)
		}
	}
	for cookie, want := range map[string]string{"": "light", "dark": "dark", "light": "light", "blue": "light"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: themeCookie, Value: cookie})
		}
		if got := themeOf(r); got != want {
			t.Errorf("themeOf(%q) = %q, want %q", cookie, got, want)
		}
	}

	// The page carries both on <html>, so the first paint is already right.
	e := newEnv(t)
	b := e.browser()
	b.Jar.SetCookies(mustURL(t, e.srv.URL), []*http.Cookie{{Name: langCookie, Value: "en"}, {Name: themeCookie, Value: "dark"}})
	if _, body, _ := e.get(b, "/login"); !strings.Contains(body, `<html lang="en" data-theme="dark"`) || !strings.Contains(body, "Sign in") {
		t.Errorf("login page with en + dark cookies:\n%.300s", body)
	}
	if _, body, _ := e.get(e.browser(), "/login"); !strings.Contains(body, `<html lang="zh-CN" data-theme="light"`) {
		t.Errorf("login page without cookies:\n%.300s", body)
	}
}

// Every UI string is written in both languages where it is used (i18n.go). This
// reads the sources, so it also covers the messages no test happens to trigger.
func TestUIStringsAreBilingual(t *testing.T) {
	// Templates: {{T "中文" "English"}} needs a real English side, and Chinese outside
	// T may only sit in the first branch of {{if zh}}…{{else}}…{{end}}.
	pair := regexp.MustCompile(`\bT ("(?:[^"\\]|\\.)*") ("(?:[^"\\]|\\.)*")`)
	zhBranch := regexp.MustCompile(`(?s)\{\{if zh\}\}.*?\{\{else\}\}`)
	comment := regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)
	files, err := fs.Glob(web.FS, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates: %v (%d files)", err, len(files))
	}
	pairs := 0
	for _, f := range files {
		src := comment.ReplaceAllString(readWeb(t, f), "")
		for _, m := range pair.FindAllStringSubmatch(src, -1) {
			pairs++
			zh, _ := strconv.Unquote(m[1])
			en, err := strconv.Unquote(m[2])
			if err != nil || strings.TrimSpace(en) == "" || cjk.MatchString(en) {
				t.Errorf("%s: T %s %s has no English text", f, m[1], m[2])
			}
			if strings.TrimSpace(zh) == "" {
				t.Errorf("%s: T %s %s has no Chinese text", f, m[1], m[2])
			}
		}
		rest := zhBranch.ReplaceAllString(pair.ReplaceAllString(src, ""), "")
		rest = strings.ReplaceAll(rest, `>中文</button>`, "") // the language switch names each language in itself
		if loc := cjk.FindStringIndex(rest); loc != nil {
			t.Errorf("%s: Chinese text outside {{T}} and {{if zh}}: …%s…", f, strings.TrimSpace(rest[max(0, loc[0]-40):min(len(rest), loc[1]+40)]))
		}
	}
	if pairs < 100 {
		t.Errorf("only %d {{T}} pairs found: the pattern no longer matches", pairs)
	}

	// Go: a Chinese string literal is the first argument of l.tr(zh, en), or the first
	// element of a two-string value such as msg{zh, en}; its partner has no Chinese.
	fset := token.NewFileSet()
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var parsed []*ast.File
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, file)
	}
	hasCJK := func(n ast.Node) (found, anyString bool) {
		ast.Inspect(n, func(x ast.Node) bool {
			if lit, ok := x.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				anyString = true
				found = found || cjk.MatchString(lit.Value)
			}
			return true
		})
		return
	}
	within := func(outer, inner ast.Node) bool { return outer.Pos() <= inner.Pos() && inner.End() <= outer.End() }
	lits := 0
	for _, file := range parsed {
		var stack []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !cjk.MatchString(lit.Value) {
				return true
			}
			lits++
			for i := len(stack) - 2; i >= 0; i-- {
				var parts []ast.Expr
				switch p := stack[i].(type) {
				case *ast.CallExpr:
					if sel, ok := p.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "tr" {
						parts = p.Args
					}
				case *ast.CompositeLit:
					if len(p.Elts) == 2 {
						parts = p.Elts
					}
				}
				if len(parts) != 2 || !within(parts[0], lit) {
					continue
				}
				if zhInEN, str := hasCJK(parts[1]); !zhInEN && str {
					return true
				}
			}
			t.Errorf("%s: %s has no English counterpart", fset.Position(lit.Pos()), lit.Value)
			return true
		})
	}
	if lits < 50 {
		t.Errorf("only %d Chinese literals found in the package: the check no longer sees them", lits)
	}
}

// The panel rendered in English shows no Chinese: every page, every dialog and
// the usual error answers.
func TestEnglishPagesHaveNoChinese(t *testing.T) {
	pe := newPagesEnv(t)
	pe.c.Jar.SetCookies(mustURL(t, pe.srv.URL), []*http.Cookie{{Name: langCookie, Value: "en"}})

	if _, _, err := pe.conf.ImportEgresses(nil, "socks5://u:p@10.0.0.1:1080 #hk-1\nsocks5://u:p@10.0.0.2:1080 #sg-1\n", egress.ImportOptions{DefaultScheme: "socks5"}, "", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pe.conf.AddKeys(nil, []confstore.KeyInput{
		{Secret: "tvly-dev-aaaaaaaaaaaaaaaa", Label: "free-01", Plan: "free", Egress: "hk-1"},
		{Secret: "tvly-dev-bbbbbbbbbbbbbbbb", Label: "typed", Plan: "paid", Egress: "socks5://u:p@10.9.9.9:1080"},
		{Secret: "exa-key-cccccccccccccccc", Label: "exa-01", Plan: "free", Egress: "direct"},
	}); err != nil {
		t.Fatal(err)
	}
	tok, _, err := pe.conf.AddToken(nil, confstore.TokenSpec{Name: "laptop", Enabled: true, Expires: "2030-01-01",
		Limits: confstore.TokenLimits{RPM: 60, DailyRequests: 100, MonthlyUSD: 2}})
	if err != nil {
		t.Fatal(err)
	}
	snap := pe.conf.Snapshot()
	kid := snap.Keys[0].ID
	now := time.Now()
	if err := pe.st.InsertRequestLogs(context.Background(), []store.RequestLog{
		{TS: now.Add(-time.Hour), RequestID: "r1", Provider: "tavily", Surface: "mcp", Operation: "search", KeyID: kid, TokenID: snap.Tokens[0].ID,
			ClientIP: "203.0.113.5", Status: 200, Outcome: "ok", LatencyMS: 120, CostCredits: 1, QueryPreview: "plain ascii query"},
		{TS: now.Add(-30 * time.Minute), RequestID: "r2", Provider: "exa", Surface: "rest", Operation: "search", ClientIP: "203.0.113.6",
			Status: 503, Outcome: "gateway_error", ErrorKind: "no_keys", LatencyMS: 3, Attempts: 2},
		{TS: now.Add(-20 * time.Minute), RequestID: "r3", Provider: "tavily", Surface: "rest", Operation: "extract", KeyID: kid, ClientIP: "203.0.113.6",
			Status: 429, Outcome: "gateway_error", ErrorKind: "rate_limited", LatencyMS: 30, Attempts: 1},
	}, []store.Attempt{{RequestID: "r2", N: 1, TS: now, KeyID: kid, Status: 502, ErrorKind: "upstream_io"}, {RequestID: "r2", N: 2, TS: now, Status: 503, ErrorKind: "no_keys"}}); err != nil {
		t.Fatal(err)
	}
	_ = tok

	textarea := regexp.MustCompile(`(?s)<textarea[^>]*>.*?</textarea>`) // file contents carry their own comments
	clean := func(what, body string) {
		t.Helper()
		body = textarea.ReplaceAllString(body, "")
		body = strings.ReplaceAll(body, `>中文</button>`, "") // the language switch names each language in itself
		if loc := cjk.FindStringIndex(body); loc != nil {
			t.Errorf("%s in English still shows Chinese: …%s…", what, strings.Join(strings.Fields(body[max(0, loc[0]-80):min(len(body), loc[1]+80)]), " "))
		}
	}

	list := pe.page("/requests")
	reqID := regexp.MustCompile(`href="/requests/(\d+)"`).FindAllStringSubmatch(list, -1)
	if len(reqID) != 3 {
		t.Fatalf("request links = %v", reqID)
	}
	dialogs := []string{"/keys/new", "/keys/" + kid, "/keys/" + snap.Keys[1].ID, "/keys/" + snap.Keys[2].ID, "/egresses/import", "/egresses/hk-1",
		"/tokens/new", "/tokens/" + snap.Tokens[0].ID, "/files/edit?f=config.yaml", "/requests/" + reqID[0][1], "/requests/" + reqID[1][1], "/security/totp/setup"}
	pages := []string{"/", "/?range=24h", "/?range=7d", "/?range=30d", "/requests", "/requests?errors=1", "/requests?live=1", "/requests/rows",
		"/keys", "/egresses", "/tokens", "/settings", "/files", "/security", "/security?source=panel"}
	for name := range flashes {
		pages = append(pages, "/keys?ok="+name)
	}
	for _, path := range append(pages, dialogs...) { // a dialog address visited directly is a full page
		body := pe.page(path)
		if strings.Contains(body, "<!doctype html>") && !strings.Contains(body, `<html lang="en"`) {
			t.Errorf("%s: not rendered as an English page", path)
		}
		clean(path, body)
	}
	for _, path := range dialogs {
		code, body, _ := pe.htmxGet(pe.c, path)
		if code != http.StatusOK || strings.Contains(body, "<!doctype html>") {
			t.Errorf("dialog %s = %d, or a whole page", path, code)
		}
		clean("dialog "+path, body)
	}

	// The error answers, as a page and as a dialog fragment.
	for _, c := range []struct {
		from, path string
		form       url.Values
		status     int
	}{
		{"/keys", "/keys/add", url.Values{"lines": {"a,b,c,d,e"}}, http.StatusBadRequest},
		{"/keys", "/keys/add", url.Values{"secret": {""}}, http.StatusBadRequest},
		{"/keys", "/keys/batch", url.Values{"action": {"disable"}}, http.StatusBadRequest},
		{"/keys", "/keys/batch", url.Values{"action": {"egress"}, "id": {kid}, "egress": {"__none__"}}, http.StatusBadRequest},
		{"/keys", "/keys/" + kid, url.Values{"priority": {"x"}}, http.StatusBadRequest},
		{"/keys", "/keys/" + kid, url.Values{"priority": {"1"}, "weight": {"0"}}, http.StatusBadRequest},
		{"/keys", "/keys/" + kid + "/reveal", nil, http.StatusOK},
		{"/egresses", "/egresses/import", url.Values{"text": {"10.0.0.3:1080 #x-1\nnot a proxy\n10.0.0.3:1080"}, "scheme": {"socks5"}, "step": {"preview"}}, http.StatusOK},
		{"/egresses", "/egresses/hk-1/delete", url.Values{}, http.StatusBadRequest},
		{"/egresses", "/egresses/hk-1/rename", url.Values{"to": {"sg-1"}}, http.StatusBadRequest},
		{"/tokens", "/tokens/add", url.Values{"name": {"x"}, "surfaces": {"mcp"}}, http.StatusBadRequest},
		{"/tokens", "/tokens/add", url.Values{"name": {"x"}, "providers": {"exa"}, "surfaces": {"mcp"}, "limit_rpm": {"-1"}}, http.StatusBadRequest},
		{"/tokens", "/tokens/add", url.Values{"name": {"second"}, "providers": {"exa"}, "surfaces": {"mcp", "rest"}, "enabled": {"on"}}, http.StatusOK},
		{"/settings", "/settings/routing", url.Values{"routing__max_attempts": {"99"}}, http.StatusBadRequest},
		{"/files", "/files/edit?f=keys/exa.yaml", url.Values{"version": {"stale"}, "content": {"keys: []\n"}}, http.StatusBadRequest},
		{"/security", "/security/password", url.Values{"current": {pw}, "new": {"short"}, "confirm": {"short"}}, http.StatusBadRequest},
		{"/security", "/security/password", url.Values{"current": {pw}, "new": {"one-long-password-1"}, "confirm": {"another-password-2"}}, http.StatusBadRequest},
		{"/security", "/security/password", url.Values{"current": {"wrong-current-pw"}, "new": {"one-long-password-1"}, "confirm": {"one-long-password-1"}}, http.StatusUnauthorized},
		{"/security", "/security/totp/enable", url.Values{"secret": {"JBSWY3DPEHPK3PXP"}, "code": {"000000"}, "current": {pw}}, http.StatusBadRequest},
		{"/security", "/security/totp/disable", url.Values{"current": {pw}, "code": {"000000"}}, http.StatusUnauthorized},
	} {
		for _, viaHTMX := range []bool{false, true} {
			form := url.Values{}
			for k, v := range c.form {
				form[k] = v
			}
			if viaHTMX && c.status == http.StatusOK && strings.HasSuffix(c.path, "/tokens/add") {
				form.Set("name", "third") // the page pass created "second"
			}
			var code int
			var body string
			if viaHTMX {
				code, body, _ = pe.htmxSubmit(c.from, c.path, form)
			} else {
				code, body, _ = pe.submit(c.from, c.path, form)
			}
			what := "POST " + c.path + " " + c.form.Encode()
			want := c.status
			if !viaHTMX && strings.Contains(c.form.Get("step"), "preview") {
				want = http.StatusSeeOther // the import preview only exists in the dialog
			}
			if code != want {
				t.Errorf("%s (htmx %v) = %d, want %d", what, viaHTMX, code, want)
			}
			clean(what, body)
		}
	}

	// Signed out: the login page and its errors.
	anon := pe.browser()
	anon.Jar.SetCookies(mustURL(t, pe.srv.URL), []*http.Cookie{{Name: langCookie, Value: "en"}})
	_, body, _ := pe.get(anon, "/login")
	clean("/login", body)
	_, body, _ = pe.post(anon, "/login", url.Values{"username": {"admin"}, "password": {"wrong-password-123"}})
	clean("a failed login", body)
}

// The TOTP setup is a dialog: htmx gets fragments and an HX-Redirect; the address
// visited directly is a page whose form posts normally.
func TestTOTPSetupDialog(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	e.login(b, pw, "")
	code, frag, _ := e.htmxGet(b, "/security/totp/setup")
	m := secretRe.FindStringSubmatch(frag)
	if code != http.StatusOK || m == nil || strings.Contains(frag, "<!doctype html>") || !strings.Contains(frag, `hx-post="/security/totp/enable"`) {
		t.Fatalf("setup dialog = %d\n%.400s", code, frag)
	}
	secret, tok := m[1], csrfRe.FindStringSubmatch(frag)[1]
	// A wrong code keeps the same secret in the dialog.
	wrong := "000000"
	if good, _ := totp.Code(secret, time.Now()); good == wrong {
		wrong = "111111"
	}
	code, frag, _ = e.htmxPost(b, "/security/totp/enable", url.Values{"csrf": {tok}, "secret": {secret}, "code": {wrong}, "current": {pw}})
	if code != http.StatusBadRequest || strings.Contains(frag, "<!doctype html>") || !strings.Contains(frag, "验证码不正确") || !strings.Contains(frag, `value="`+secret+`"`) {
		t.Fatalf("wrong code in the dialog = %d\n%.400s", code, frag)
	}
	good, _ := totp.Code(secret, time.Now())
	code, _, h := e.htmxPost(b, "/security/totp/enable", url.Values{"csrf": {tok}, "secret": {secret}, "code": {good}, "current": {pw}})
	if code != http.StatusNoContent || h.Get("HX-Redirect") != "/security?ok=totp_on" {
		t.Fatalf("enable from the dialog = %d, HX-Redirect %q", code, h.Get("HX-Redirect"))
	}
	if e.conf.Snapshot().Config.Admin.TOTPSecret != secret {
		t.Fatal("secret not saved")
	}
	if code, body, _ := e.get(b, "/security?ok=totp_on"); code != http.StatusOK || !strings.Contains(body, "两步验证已开启") {
		t.Fatalf("the session did not survive enabling TOTP: %d", code)
	}
}

// A dialog address opened directly (a bookmark, a reload) is never a bare fragment:
// it is a page, and its forms post without htmx so nothing is swapped into a dialog
// that would then hold a second copy of the same form ids.
func TestDialogAddressesVisitedDirectly(t *testing.T) {
	pe := newPagesEnv(t)
	if _, _, err := pe.conf.AddKeys(nil, []confstore.KeyInput{{Secret: "tvly-dev-aaaaaaaaaaaaaaaa", Label: "free-01"}}); err != nil {
		t.Fatal(err)
	}
	kid := pe.conf.Snapshot().Keys[0].ID
	for path, want := range map[string]string{"/keys/new": `data-modal="/keys/new"`, "/tokens/new": `data-modal="/tokens/new"`, "/egresses/import": `data-modal="/egresses/import"`} {
		if body := pe.page(path); !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, want) {
			t.Errorf("%s visited directly is not its list page", path)
		}
	}
	page := pe.page("/keys/" + kid)
	if !strings.Contains(page, "<!doctype html>") || !strings.Contains(page, `id="key-form" method="post" action="/keys/`+kid+`"`) {
		t.Fatalf("key page is not a page with a plain form:\n%.300s", page)
	}
	if strings.Contains(page[strings.Index(page, `<main`):strings.Index(page, `</main>`)], "hx-post") {
		t.Error("the key page's forms still post into the dialog")
	}
	if _, frag, _ := pe.htmxGet(pe.c, "/keys/"+kid); !strings.Contains(frag, `hx-post="/keys/`+kid+`" hx-target="#modal-body"`) || !strings.Contains(frag, `method="post"`) {
		t.Errorf("the key dialog's form does not post through htmx (with a plain POST as its fallback):\n%.300s", frag)
	}
}
