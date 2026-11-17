package adminauth

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/sodiseng/search-warden/gateway/internal/httpx"
)

type ctxKey struct{}

// FromContext returns the session set by Require (nil outside protected routes).
func FromContext(ctx context.Context) *Session {
	s, _ := ctx.Value(ctxKey{}).(*Session)
	return s
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// maxBody bounds panel form/JSON posts (node import pastes included).
const maxBody = 2 << 20

// Require protects a handler: it needs a valid session, and every
// state-changing request must also pass the Origin check and carry the
// session's CSRF token (X-CSRF-Token header or "csrf" form field).
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := a.Authenticate(r)
		if err != nil {
			a.log.Error("session lookup failed", "err", err)
			httpx.WriteJSONError(w, http.StatusInternalServerError, "internal_error", "session lookup failed")
			return
		}
		if s == nil {
			a.unauthorized(w, r)
			return
		}
		if !safeMethod(r.Method) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			if !a.CheckOrigin(r) {
				a.log.Warn("cross-origin write refused", "method", r.Method, "path", r.URL.Path, "detail", a.OriginDetail(r))
				httpx.WriteJSONError(w, http.StatusForbidden, "forbidden", "cross-origin request rejected")
				return
			}
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" {
				tok = r.PostFormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.CSRF)) != 1 {
				httpx.WriteJSONError(w, http.StatusForbidden, "forbidden", "missing or invalid CSRF token; reload the page")
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s)))
	})
}

// LoginURL is where unauthenticated browsers are sent.
func (a *Auth) LoginURL() string { return a.opts.BasePath + "login" }

func (a *Auth) unauthorized(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Header.Get("HX-Request") == "true":
		w.Header().Set("HX-Redirect", a.LoginURL())
		httpx.WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "login required")
	case safeMethod(r.Method) && strings.Contains(r.Header.Get("Accept"), "text/html"):
		next := a.opts.BasePath + strings.TrimPrefix(r.URL.Path, "/")
		if r.URL.RawQuery != "" {
			next += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, a.LoginURL()+"?next="+url.QueryEscape(next), http.StatusSeeOther)
	default:
		httpx.WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "login required")
	}
}

// CheckOrigin rejects cross-site state-changing requests. Browsers send
// Sec-Fetch-Site and Origin on POST; non-browser clients without them still
// need the session cookie and CSRF token.
//
// Accepted: the request's own host, the configured SMAIO/WARDEN_ADMIN_PUBLIC_URL,
// and — for a panel reached on a loopback address — any loopback Origin.
// localhost and 127.0.0.1 are the same panel, and a stale public URL must not
// lock the operator out of the panel on their own machine (the MCP endpoint
// applies the same rule, ARCHITECTURE.md §8.2). Found in use on 2026-11-12:
// http://127.0.0.1:18684 with Origin http://localhost:18684 was refused.
func (a *Auth) CheckOrigin(r *http.Request) bool {
	sfs := r.Header.Get("Sec-Fetch-Site")
	if sfs != "" && sfs != "same-origin" && sfs != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	// "Origin: null": the browser could not name the document's origin (an opaque
	// origin — a saved copy of the page, a sandboxed frame, or a browser privacy
	// mode). Seen in use on 2026-11-12: a normal login from http://127.0.0.1:18684
	// sent "null" with "Sec-Fetch-Site: same-origin". Since Sec-Fetch-Site is set by
	// the browser and an attacker's cross-site form can only produce "cross-site",
	// trusting "null" when it says same-origin (or none) is safe; the session cookie
	// is SameSite=Strict and every write also needs the session's CSRF token.
	if strings.EqualFold(origin, "null") {
		return sfs == "same-origin" || sfs == "none"
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if a.opts.PublicURL != "" {
		if p, err := url.Parse(a.opts.PublicURL); err == nil && strings.EqualFold(u.Scheme, p.Scheme) && strings.EqualFold(u.Host, p.Host) {
			return true
		}
	}
	return isLoopbackHost(u.Hostname()) && isLoopbackHost(hostOnly(r.Host))
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(h, "[]"))
	return err == nil && a.Unmap().IsLoopback()
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// OriginDetail describes why a state-changing request was refused. It is shown to
// the operator (whose own browser sent it) and written to the log, because the
// generic message alone left the first-run failure undiagnosable. It only names
// headers and the setting, so it reads the same in either panel language.
func (a *Auth) OriginDetail(r *http.Request) string {
	origin := r.Header.Get("Origin")
	if strings.EqualFold(origin, "null") {
		origin = "null (opaque, judged by Sec-Fetch-Site)"
	}
	parts := []string{"Origin: " + orNone(origin), "Host: " + orNone(r.Host)}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
		parts = append(parts, "Sec-Fetch-Site: "+sfs)
	}
	parts = append(parts, "WARDEN_ADMIN_PUBLIC_URL: "+orNone(a.opts.PublicURL))
	return strings.Join(parts, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// SafeNext validates a post-login redirect target: only paths inside the panel
// (no scheme, no host, no protocol-relative "//"), else the panel root.
func (a *Auth) SafeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, a.opts.BasePath) || strings.HasPrefix(next, "//") ||
		strings.ContainsAny(next, "\\\r\n") {
		return a.opts.BasePath
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" {
		return a.opts.BasePath
	}
	return next
}
