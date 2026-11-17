package admin

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/sodiseng/search-warden/gateway/web"
)

// Found in use on 2026-11-12: on the login page an error message is rendered above
// the card, and the card jumped to the left because the CSS centered only an
// `:only-child` card. The standalone pages therefore carry an explicit class.
func TestStandalonePagesKeepCenteredCard(t *testing.T) {
	css := readWeb(t, "static/app.css")
	if !strings.Contains(css, ".card.center { margin-left: auto; margin-right: auto; }") {
		t.Error("app.css lost the .card.center rule")
	}
	// The old rule centered the card only while it was an :only-child of <main>; a flash or
	// error message above it broke the centering.
	if strings.Contains(css, ":only-child") {
		t.Error("app.css centers something by :only-child again")
	}
	login := readWeb(t, "templates/login.html")
	if !strings.Contains(login, `class="card center login-card"`) {
		t.Error("login.html does not center its card")
	}
	// The message belongs inside the card: above it, it shifted the card.
	card := login[strings.Index(login, `<section class="card center login-card">`):strings.Index(login, `</section>`)]
	if !strings.Contains(card, `{{if .Error}}<div class="alert err" role="alert">`) {
		t.Error("login.html does not render its error inside the card")
	}
	// The TOTP setup is a dialog like every other create/generate flow.
	totp := readWeb(t, "templates/totp_setup.html")
	if !strings.Contains(totp, `{{define "modal"}}`) || !strings.Contains(totp, `{{define "totpbody"}}`) {
		t.Error("totp_setup.html is not a dialog with a page fallback")
	}
	// Every other page gets its error from the layout: an alert that stays in the page.
	layout := readWeb(t, "templates/layout.html")
	if !strings.Contains(layout, `{{define "pageerror"}}{{if and .Error (not .FlashInCard)}}<div class="alert err" role="alert">`) {
		t.Error("layout.html lost the error block")
	}
	// A confirmation is a toast outside the page flow, so it cannot shift the page either.
	if !strings.Contains(layout, "<body>\n{{template \"toast\" .}}") {
		t.Error("layout.html no longer renders the toast outside the page content")
	}
}

// Every create / edit / generate flow is a dialog: its template has a "modal"
// block next to the full-page fallback, and the fragment carries the dialog's
// header (which holds the close button).
func TestDialogTemplates(t *testing.T) {
	for _, name := range []string{"keys", "key", "egresses", "egress", "tokens", "token", "secret", "file", "request", "totp_setup"} {
		src := readWeb(t, "templates/"+name+".html")
		i := strings.Index(src, `{{define "modal"}}`)
		if i < 0 {
			t.Errorf("%s.html has no modal block", name)
			continue
		}
		if block := src[i:]; !strings.Contains(block[:strings.Index(block, "\n{{end}}\n")+1], `{{template "mhead"`) {
			t.Errorf("%s.html: the modal block has no dialog header", name)
		}
	}
}

// The strict CSP (style-src 'self', script-src 'self') leaves no room for inline
// styles, scripts or event handlers, and nothing may be loaded from elsewhere.
func TestTemplatesObeyTheCSP(t *testing.T) {
	inline := regexp.MustCompile(`(?i)\sstyle\s*=|<style|\son[a-z]+\s*=|<script(?:\s[^>]*)?>\s*[^<\s]|(?:src|href)\s*=\s*"(?:https?:)?//`)
	files, err := fs.Glob(web.FS, "templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("templates: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		if m := inline.FindString(readWeb(t, f)); m != "" {
			t.Errorf("%s: %q is not allowed by the panel's CSP", f, m)
		}
	}
	css := readWeb(t, "static/app.css")
	if m := regexp.MustCompile(`(?i)url\(\s*["']?(?:https?:)?//|@import`).FindString(css); m != "" {
		t.Errorf("app.css loads something from outside: %q", m)
	}
}

// Every {{icon "name"}} must exist in the vendored sprite: a missing symbol
// renders as nothing, which no other test would notice.
func TestIconsExistInSprite(t *testing.T) {
	sprite := readWeb(t, iconSprite)
	have := map[string]bool{}
	for _, m := range regexp.MustCompile(`<symbol id="([^"]+)"`).FindAllStringSubmatch(sprite, -1) {
		have[m[1]] = true
	}
	if len(have) == 0 {
		t.Fatal("no symbols in the sprite")
	}
	if strings.Contains(sprite, "<script") || strings.Contains(sprite, "style=") {
		t.Error("the sprite carries a script or an inline style")
	}
	files, _ := fs.Glob(web.FS, "templates/*.html")
	used := 0
	for _, f := range files {
		for _, m := range regexp.MustCompile(`\{\{icon "([^"]+)"\}\}`).FindAllStringSubmatch(readWeb(t, f), -1) {
			used++
			if !have[m[1]] {
				t.Errorf("%s uses icon %q, which is not in %s", f, m[1], iconSprite)
			}
		}
	}
	if used == 0 {
		t.Error("no icon found in the templates: the pattern no longer matches")
	}
}

// Static assets are revalidated on every view (Cache-Control: no-cache); the ETag
// is what makes that cheap. Their types are pinned so nosniff never breaks them.
func TestStaticAssetsRevalidate(t *testing.T) {
	e := newEnv(t)
	for path, ctype := range map[string]string{
		"/static/app.css": "text/css",
		"/static/app.js":  "text/javascript",
		"/static/vendor/pico-2.1.1.indigo.min.css":     "text/css",
		"/static/vendor/htmx-2.0.4.min.js":             "text/javascript",
		"/static/vendor/lucide-1.72.0.svg":             "image/svg+xml",
		"/static/vendor/inter-latin-wght-normal.woff2": "font/woff2",
	} {
		code, _, h := e.get(e.browser(), path)
		tag := h.Get("ETag")
		if code != http.StatusOK || !strings.HasPrefix(h.Get("Content-Type"), ctype) || h.Get("Cache-Control") != "no-cache" || tag == "" {
			t.Errorf("%s = %d, type %q, cache %q, etag %q", path, code, h.Get("Content-Type"), h.Get("Cache-Control"), tag)
			continue
		}
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		req.Header.Set("If-None-Match", tag)
		resp, err := e.browser().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotModified {
			t.Errorf("%s with its ETag = %d, want 304", path, resp.StatusCode)
		}
	}
	if code, _, h := e.get(e.browser(), "/static/no-such.css"); code != http.StatusNotFound || h.Get("ETag") != "" {
		t.Errorf("missing asset = %d, etag %q", code, h.Get("ETag"))
	}
}

func readWeb(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(web.FS, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
