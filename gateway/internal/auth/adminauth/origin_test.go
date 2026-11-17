package adminauth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Found in use on 2026-11-12: opening http://127.0.0.1:18684 and logging in was
// refused with "请求来源不正确" because the browser sent a loopback Origin that did
// not match the Host character for character (localhost vs 127.0.0.1), or because a
// stale WARDEN_ADMIN_PUBLIC_URL was still set.
func TestCheckOriginLoopback(t *testing.T) {
	for _, c := range []struct {
		name, host, origin, publicURL, site string
		want                                bool
	}{
		{"same host", "127.0.0.1:18684", "http://127.0.0.1:18684", "", "same-origin", true},
		{"localhost origin, ip host", "127.0.0.1:18684", "http://localhost:18684", "", "same-origin", true},
		{"ip origin, localhost host", "localhost:18684", "http://127.0.0.1:18684", "", "same-origin", true},
		{"ipv6 loopback", "[::1]:18684", "http://127.0.0.1:18684", "", "same-origin", true},
		{"other loopback port", "127.0.0.1:18684", "http://127.0.0.1:18081", "", "same-origin", true},
		{"stale public url, local access", "127.0.0.1:18684", "http://127.0.0.1:18684", "https://admin.example.com", "same-origin", true},
		{"foreign origin", "127.0.0.1:18684", "http://evil.example", "", "same-origin", false},
		{"foreign origin, no sec-fetch", "127.0.0.1:18684", "http://evil.example", "", "", false},
		{"cross-site", "127.0.0.1:18684", "http://127.0.0.1:18684", "", "cross-site", false},
		{"public url matches", "warden:18684", "https://admin.example.com", "https://admin.example.com", "same-origin", true},
		{"public url scheme mismatch", "warden:18684", "http://admin.example.com", "https://admin.example.com", "same-origin", false},
		{"behind a proxy, loopback origin is not the panel", "admin.example.com", "http://127.0.0.1:18684", "", "same-origin", false},
		{"no origin (non-browser client)", "127.0.0.1:18684", "", "", "", true},
		// Seen in use 2026-11-12: the browser sent "Origin: null" for a same-origin login.
		{"opaque origin, same-origin", "127.0.0.1:18684", "null", "", "same-origin", true},
		{"opaque origin, no sec-fetch", "127.0.0.1:18684", "null", "", "", false},
		{"opaque origin, cross-site", "127.0.0.1:18684", "null", "", "cross-site", false},
		{"opaque origin on a public panel", "admin.example.com", "null", "https://admin.example.com", "same-origin", true},
		{"unparseable origin", "127.0.0.1:18684", "not a url", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, Options{PublicURL: c.publicURL})
			r := httptest.NewRequest("POST", "/login", strings.NewReader(""))
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.site != "" {
				r.Header.Set("Sec-Fetch-Site", c.site)
			}
			if got := f.a.CheckOrigin(r); got != c.want {
				t.Errorf("CheckOrigin(host=%q origin=%q public=%q site=%q) = %v, want %v",
					c.host, c.origin, c.publicURL, c.site, got, c.want)
			}
			if !c.want {
				if d := f.a.OriginDetail(r); !strings.Contains(d, "Host") || !strings.Contains(d, "Origin") {
					t.Errorf("OriginDetail = %q, want it to name both sides", d)
				}
			}
		})
	}
}
