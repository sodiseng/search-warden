package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustResolver(t *testing.T, trusted, headers []string) *ClientIPResolver {
	t.Helper()
	r, err := NewClientIPResolver(trusted, headers)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func req(remote string, hdr map[string][]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for k, vs := range hdr {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return r
}

func TestClientIPResolve(t *testing.T) {
	def := mustResolver(t, DefaultTrustedProxies(), DefaultClientIPHeaders())
	all := mustResolver(t, DefaultTrustedProxies(), []string{"X-Forwarded-For", "Forwarded", "X-Real-IP", "CF-Connecting-IP"})

	tests := []struct {
		name       string
		r          *ClientIPResolver
		remote     string
		hdr        map[string][]string
		wantIP     string
		wantSource string
	}{
		{"direct client, no headers", def, "203.0.113.7:5555", nil, "203.0.113.7", "remote"},
		{"untrusted peer cannot spoof XFF", def, "203.0.113.7:5555",
			map[string][]string{"X-Forwarded-For": {"1.1.1.1"}}, "203.0.113.7", "remote"},
		{"trusted proxy, single hop", def, "172.18.0.2:4000",
			map[string][]string{"X-Forwarded-For": {"198.51.100.9"}}, "198.51.100.9", HeaderXForwardedFor},
		{"client-forged leftmost entry is ignored", def, "127.0.0.1:4000",
			map[string][]string{"X-Forwarded-For": {"6.6.6.6, 198.51.100.9"}}, "198.51.100.9", HeaderXForwardedFor},
		{"skips trusted inner hops", def, "10.0.0.5:80",
			map[string][]string{"X-Forwarded-For": {"198.51.100.9, 10.0.0.9, 192.168.1.1"}}, "198.51.100.9", HeaderXForwardedFor},
		{"multiple XFF header lines are concatenated", def, "10.0.0.5:80",
			map[string][]string{"X-Forwarded-For": {"6.6.6.6", "198.51.100.9"}}, "198.51.100.9", HeaderXForwardedFor},
		{"garbage hop aborts to remote", def, "10.0.0.5:80",
			map[string][]string{"X-Forwarded-For": {"198.51.100.9, not-an-ip"}}, "10.0.0.5", "remote"},
		{"all hops trusted -> leftmost", def, "10.0.0.5:80",
			map[string][]string{"X-Forwarded-For": {"192.168.1.20, 10.0.0.9"}}, "192.168.1.20", HeaderXForwardedFor},
		{"XFF entry with port", def, "127.0.0.1:1",
			map[string][]string{"X-Forwarded-For": {"198.51.100.9:443"}}, "198.51.100.9", HeaderXForwardedFor},
		{"IPv6 client", def, "[::1]:8080",
			map[string][]string{"X-Forwarded-For": {"2001:db8::1"}}, "2001:db8::1", HeaderXForwardedFor},
		{"IPv4-mapped peer is unmapped and trusted", def, "[::ffff:127.0.0.1]:9",
			map[string][]string{"X-Forwarded-For": {"198.51.100.9"}}, "198.51.100.9", HeaderXForwardedFor},
		{"X-Real-IP ignored by default (spoofable through Caddy)", def, "127.0.0.1:1",
			map[string][]string{"X-Real-Ip": {"6.6.6.6"}}, "127.0.0.1", "remote"},
		{"CF-Connecting-IP ignored by default", def, "127.0.0.1:1",
			map[string][]string{"Cf-Connecting-Ip": {"6.6.6.6"}}, "127.0.0.1", "remote"},
		{"Forwarded header, quoted IPv6 with port", all, "127.0.0.1:1",
			map[string][]string{"Forwarded": {`for=6.6.6.6, for="[2001:db8::7]:4711";proto=https`}}, "2001:db8::7", HeaderForwarded},
		{"opt-in X-Real-IP used when XFF absent", all, "127.0.0.1:1",
			map[string][]string{"X-Real-Ip": {"198.51.100.3"}}, "198.51.100.3", HeaderXRealIP},
		{"repeated single-value header rejected", all, "127.0.0.1:1",
			map[string][]string{"X-Real-Ip": {"1.1.1.1", "2.2.2.2"}}, "127.0.0.1", "remote"},
		{"unparseable remote", def, "garbage", nil, "", "remote"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.r.Resolve(req(tc.remote, tc.hdr))
			if got.String() != tc.wantIP || got.Source != tc.wantSource {
				t.Errorf("got %q via %q, want %q via %q", got.String(), got.Source, tc.wantIP, tc.wantSource)
			}
		})
	}
}

func TestNewClientIPResolverValidation(t *testing.T) {
	if _, err := NewClientIPResolver([]string{"10.0.0.0/8", "203.0.113.5"}, nil); err != nil {
		t.Errorf("bare IP should be accepted: %v", err)
	}
	if _, err := NewClientIPResolver([]string{"nope"}, nil); err == nil {
		t.Error("invalid CIDR should fail")
	}
	if _, err := NewClientIPResolver(nil, []string{"X-Client-Ip"}); err == nil {
		t.Error("unsupported header should fail")
	}
}
