package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Found in acceptance (2026-11-10): WARDEN_PUBLIC_URL was read at startup but the
// page that shows a new token still printed a placeholder instead of the address.
func TestNewTokenPageShowsGatewayURL(t *testing.T) {
	form := func(name string) url.Values {
		return url.Values{"name": {name}, "providers": {"tavily", "exa"}, "surfaces": {"mcp", "rest"}, "key_group": {"all"}, "enabled": {"on"}}
	}
	withURL := newEnv(t, func(o *Options) { o.GatewayURL = "https://gw.example.com/" })
	c := withURL.browser()
	if code := withURL.login(c, pw, ""); code != http.StatusSeeOther {
		t.Fatalf("login = %d", code)
	}
	f := form("a")
	f.Set("csrf", withURL.csrf(c, "/tokens"))
	code, body, _ := withURL.post(c, "/tokens/add", f)
	if code != http.StatusOK {
		t.Fatalf("create = %d", code)
	}
	for _, want := range []string{
		"https://gw.example.com/tavily/mcp?tavilyApiKey=", "https://gw.example.com/exa/mcp</code>",
		"https://gw.example.com/mcp</code>", "<code>https://gw.example.com/tavily</code>", "<code>https://gw.example.com/exa</code>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("token page lacks %q", want)
		}
	}
	if strings.Contains(body, "网关地址") {
		t.Error("the placeholder is still shown although the public URL is known")
	}

	plain := newEnv(t)
	c = plain.browser()
	if code := plain.login(c, pw, ""); code != http.StatusSeeOther {
		t.Fatalf("login = %d", code)
	}
	f = form("b")
	f.Set("csrf", plain.csrf(c, "/tokens"))
	if _, body, _ = plain.post(c, "/tokens/add", f); !strings.Contains(body, "https://&lt;网关地址&gt;/tavily/mcp") || !strings.Contains(body, "<code>/exa</code>") {
		t.Error("without a public URL the page must show the placeholder")
	}
}
