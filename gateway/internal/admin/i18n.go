package admin

import (
	"net/http"
	"strings"
)

// The panel is bilingual (ARCHITECTURE.md §16). Every UI string is written once,
// in both languages, right where it is used: {{T "中文" "English"}} or
// {{if zh}}…{{else}}…{{end}} in templates, l.tr("中文", "English") in handlers.
// There is no catalog to keep in sync; TestEnglishPagesHaveNoChinese renders the
// panel in English and fails on any string that was left untranslated.

type lang string

const (
	langZH lang = "zh"
	langEN lang = "en"
)

var langs = []lang{langZH, langEN}

// Display preferences live in cookies the page's own script writes (app.js), so
// the server can render the right language and theme on the first paint without
// an inline script. They are not secrets and are only ever matched against the
// fixed values below.
const (
	langCookie  = "warden_lang"
	themeCookie = "warden_theme"
)

// tr picks the string of this language.
func (l lang) tr(zh, en string) string {
	if l == langEN {
		return en
	}
	return zh
}

// html is the value of <html lang>.
func (l lang) html() string {
	if l == langEN {
		return "en"
	}
	return "zh-CN"
}

// msg is a UI string kept in a table (flash messages, setting labels).
type msg struct{ ZH, EN string }

func (m msg) in(l lang) string { return l.tr(m.ZH, m.EN) }

// langOf is the request's language: the cookie set by the language switch, else
// the browser's first supported Accept-Language, else Chinese.
func langOf(r *http.Request) lang {
	if c, err := r.Cookie(langCookie); err == nil {
		switch lang(c.Value) {
		case langZH, langEN:
			return lang(c.Value)
		}
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		primary, _, _ := strings.Cut(strings.ToLower(tag), "-")
		switch primary {
		case "zh":
			return langZH
		case "en":
			return langEN
		}
	}
	return langZH
}

// themeOf is the colour theme: light unless the theme switch chose dark.
func themeOf(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil && c.Value == "dark" {
		return "dark"
	}
	return "light"
}
