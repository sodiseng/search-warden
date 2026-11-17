package clientauth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/token"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
)

func TestExtract(t *testing.T) {
	const tok = "sw_client"
	cases := []struct {
		name string
		mod  func(r *http.Request)
		url  string
		body string
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }, "/", ""},
		{"bearer lower-case scheme", func(r *http.Request) { r.Header.Set("Authorization", "bearer "+tok) }, "/", ""},
		{"x-api-key", func(r *http.Request) { r.Header.Set("x-api-key", tok) }, "/", ""},
		{"tavily url param", nil, "/?tavilyApiKey=" + tok, ""},
		{"exa url param", nil, "/?exaApiKey=" + tok, ""},
		{"merged url param", nil, "/?key=" + tok, ""},
		{"body api_key", nil, "/", `{"api_key":"` + tok + `","query":"x"}`},
		{"same token twice is fine", func(r *http.Request) { r.Header.Set("x-api-key", tok) }, "/?key=" + tok, ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", c.url, nil)
		if c.mod != nil {
			c.mod(r)
		}
		got, err := Extract(r, []byte(c.body))
		if err != nil || got != tok {
			t.Errorf("%s: %q %v", c.name, got, err)
		}
	}
	if _, err := Extract(httptest.NewRequest("POST", "/", nil), nil); err == nil {
		t.Error("missing token accepted")
	} else if d, ok := IsDenied(err); !ok || d.Status != 401 {
		t.Errorf("missing: %v", err)
	}
	r := httptest.NewRequest("POST", "/?key=other", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	if _, err := Extract(r, nil); err == nil {
		t.Error("conflicting tokens accepted")
	} else if d, _ := IsDenied(err); d.Status != 400 {
		t.Errorf("conflict status: %v", d)
	}
}

func snapWith(tokens ...confstore.Token) *confstore.Snapshot {
	return &confstore.Snapshot{Tokens: tokens}
}

func TestAuthorize(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	ip := netip.MustParseAddr("198.51.100.7")
	base := confstore.Token{Name: "a", Hash: token.Hash("good-token"), Enabled: true, Providers: []string{"tavily", "exa"}, Surfaces: []string{"mcp", "rest"}}

	if tk, err := Authorize(snapWith(base), "good-token", "tavily", "rest", ip, now); err != nil || tk.Name != "a" {
		t.Fatalf("valid: %v", err)
	}
	mod := func(f func(*confstore.Token)) *confstore.Snapshot { c := base; f(&c); return snapWith(c) }
	cases := []struct {
		name     string
		snap     *confstore.Snapshot
		tok      string
		provider string
		surface  string
		status   int
	}{
		{"unknown token", snapWith(base), "bad-token", "tavily", "rest", 401},
		{"disabled", mod(func(c *confstore.Token) { c.Enabled = false }), "good-token", "tavily", "rest", 401},
		{"expired", mod(func(c *confstore.Token) { c.Expires = now.Add(-time.Hour) }), "good-token", "tavily", "rest", 401},
		{"wrong provider", mod(func(c *confstore.Token) { c.Providers = []string{"exa"} }), "good-token", "tavily", "rest", 403},
		{"wrong surface", mod(func(c *confstore.Token) { c.Surfaces = []string{"mcp"} }), "good-token", "tavily", "rest", 403},
		{"ip not allowed", mod(func(c *confstore.Token) { c.IPAllow = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")} }), "good-token", "tavily", "rest", 403},
	}
	for _, c := range cases {
		_, err := Authorize(c.snap, c.tok, c.provider, c.surface, ip, now)
		if d, ok := IsDenied(err); !ok || d.Status != c.status {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	allowed := mod(func(c *confstore.Token) { c.IPAllow = []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")} })
	if _, err := Authorize(allowed, "good-token", "tavily", "rest", ip, now); err != nil {
		t.Errorf("allowed ip: %v", err)
	}
}

func TestStripQuery(t *testing.T) {
	cases := map[string]string{
		"":                                   "",
		"tavilyApiKey=x":                     "",
		"exaApiKey=x&tools=web_search_exa":   "tools=web_search_exa",
		"a=1&KEY=x&b=2":                      "a=1&b=2",
		"api%5Fkey=x&dynamicHighlights=true": "dynamicHighlights=true",
	}
	for in, want := range cases {
		if got := StripQuery(in); got != want {
			t.Errorf("StripQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
