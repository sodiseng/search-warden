package httpx

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

// Header names understood by ClientIPResolver.
const (
	HeaderXForwardedFor  = "X-Forwarded-For"
	HeaderForwarded      = "Forwarded"
	HeaderXRealIP        = "X-Real-Ip"
	HeaderCFConnectingIP = "Cf-Connecting-Ip"
	HeaderTrueClientIP   = "True-Client-Ip"
)

// DefaultTrustedProxies covers loopback and private ranges so that a reverse
// proxy on the same host or Docker network works with zero configuration
// (ARCHITECTURE.md §12).
func DefaultTrustedProxies() []string {
	return []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
}

// DefaultClientIPHeaders only trusts X-Forwarded-For (parsed right to left).
// X-Real-IP / CF-Connecting-IP are opt-in: proxies such as Caddy forward a
// client-supplied copy untouched, which would let clients spoof their IP.
func DefaultClientIPHeaders() []string { return []string{HeaderXForwardedFor} }

// ClientIP is the resolved caller address plus how it was derived.
type ClientIP struct {
	Addr   netip.Addr // best-effort client address (zero if RemoteAddr is unparseable)
	Remote netip.Addr // the TCP peer
	Source string     // "remote" or the header name the address came from
}

func (c ClientIP) String() string {
	if !c.Addr.IsValid() {
		return ""
	}
	return c.Addr.String()
}

// ClientIPResolver derives the client IP from the TCP peer and, only when the
// peer is a trusted proxy, from forwarding headers.
type ClientIPResolver struct {
	trusted []netip.Prefix
	headers []string
}

// NewClientIPResolver validates CIDRs (bare IPs are accepted as /32 or /128) and header names.
func NewClientIPResolver(trustedCIDRs, headers []string) (*ClientIPResolver, error) {
	r := &ClientIPResolver{}
	for _, s := range trustedCIDRs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			r.trusted = append(r.trusted, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q is not a CIDR or IP", s)
		}
		a = a.Unmap()
		r.trusted = append(r.trusted, netip.PrefixFrom(a, a.BitLen()))
	}
	for _, h := range headers {
		ch := http.CanonicalHeaderKey(strings.TrimSpace(h))
		switch ch {
		case HeaderXForwardedFor, HeaderForwarded, HeaderXRealIP, HeaderCFConnectingIP, HeaderTrueClientIP:
			r.headers = append(r.headers, ch)
		default:
			return nil, fmt.Errorf("unsupported client IP header %q", h)
		}
	}
	return r, nil
}

func (r *ClientIPResolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolve returns the client IP for req.
func (r *ClientIPResolver) Resolve(req *http.Request) ClientIP {
	remote, ok := parseHostIP(req.RemoteAddr)
	res := ClientIP{Addr: remote, Remote: remote, Source: "remote"}
	if !ok || !r.isTrusted(remote) {
		return res
	}
	for _, h := range r.headers {
		vals := req.Header.Values(h)
		if len(vals) == 0 {
			continue
		}
		var (
			a     netip.Addr
			found bool
		)
		switch h {
		case HeaderXForwardedFor:
			a, found = r.rightmostUntrusted(splitList(vals))
		case HeaderForwarded:
			a, found = r.rightmostUntrusted(forwardedFor(vals))
		default:
			// Single-value headers: exactly one occurrence, one value.
			if len(vals) == 1 && !strings.Contains(vals[0], ",") {
				a, found = parseHostIP(strings.TrimSpace(vals[0]))
			}
		}
		if found {
			res.Addr, res.Source = a, h
			return res
		}
	}
	return res
}

// rightmostUntrusted walks a hop list from the nearest hop outward, skipping
// trusted proxies. An unparseable hop aborts (the chain cannot be trusted
// beyond it). If every hop is trusted, the leftmost (originating) one wins.
func (r *ClientIPResolver) rightmostUntrusted(hops []string) (netip.Addr, bool) {
	var last netip.Addr
	for i := len(hops) - 1; i >= 0; i-- {
		a, ok := parseHostIP(hops[i])
		if !ok {
			return netip.Addr{}, false
		}
		if !r.isTrusted(a) {
			return a, true
		}
		last = a
	}
	return last, last.IsValid()
}

// splitList flattens comma-separated header values across repeated header lines, in order.
func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// forwardedFor extracts the for= parameter of each RFC 7239 element, in order.
// Elements without for= yield "" so that the walk aborts on them.
func forwardedFor(vals []string) []string {
	var out []string
	for _, elem := range splitList(vals) {
		v := ""
		for _, pair := range strings.Split(elem, ";") {
			k, val, ok := strings.Cut(strings.TrimSpace(pair), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "for") {
				v = strings.Trim(strings.TrimSpace(val), `"`)
			}
		}
		out = append(out, v)
	}
	return out
}

// parseHostIP accepts "ip", "ip:port", "[ipv6]" and "[ipv6]:port"; it unmaps IPv4-in-IPv6 and drops zones.
func parseHostIP(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")); err == nil {
		return a.Unmap().WithZone(""), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	return netip.Addr{}, false
}
