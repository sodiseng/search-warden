// Package clientauth authenticates downstream callers of the gateway
// (ARCHITECTURE.md §4.1 step 2, §11.2): it finds the token wherever official
// clients put their API key, then checks it against tokens.yaml.
package clientauth

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/sodiseng/search-warden/gateway/internal/auth/token"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
)

// Denied is an authentication or authorization failure.
type Denied struct {
	Status int
	Code   string
	Msg    string
}

func (d *Denied) Error() string { return d.Msg }

var (
	errMissing  = &Denied{http.StatusUnauthorized, "unauthorized", "missing API token: send your gateway token as a Bearer header, x-api-key, or the URL parameter"}
	errInvalid  = &Denied{http.StatusUnauthorized, "unauthorized", "invalid or expired API token"}
	errConflict = &Denied{http.StatusBadRequest, "conflicting_tokens", "different API tokens were supplied in different places"}
)

// QueryParams are the URL parameters that may carry a token; never forwarded upstream.
var QueryParams = []string{"tavilyApiKey", "exaApiKey", "key", "api_key", "apiKey"}

// Extract returns the caller's token. All supplied copies must agree.
func Extract(r *http.Request, body []byte) (string, error) {
	var found []string
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" {
			found = append(found, v)
		}
	}
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
		add(a[7:])
	}
	add(r.Header.Get("X-Api-Key"))
	q := r.URL.Query()
	for _, p := range QueryParams {
		add(q.Get(p))
	}
	if len(body) > 0 && gjson.ValidBytes(body) {
		add(gjson.GetBytes(body, "api_key").String())
		add(gjson.GetBytes(body, "apiKey").String())
	}
	if len(found) == 0 {
		return "", errMissing
	}
	for _, v := range found[1:] {
		if v != found[0] {
			return "", errConflict
		}
	}
	return found[0], nil
}

// Authorize checks a token against the configuration for one provider and
// surface ("mcp" or "rest").
func Authorize(snap *confstore.Snapshot, tok, provider, surface string, ip netip.Addr, now time.Time) (*confstore.Token, error) {
	t, ok := snap.TokenByHash(token.Hash(tok))
	if !ok || !t.Enabled || (!t.Expires.IsZero() && !now.Before(t.Expires)) {
		return nil, errInvalid
	}
	if provider != "" && !slices.Contains(t.Providers, provider) {
		return nil, &Denied{http.StatusForbidden, "forbidden", "this token is not allowed to use " + provider}
	}
	if !slices.Contains(t.Surfaces, surface) {
		return nil, &Denied{http.StatusForbidden, "forbidden", "this token is not allowed to use the " + surface + " interface"}
	}
	if len(t.IPAllow) > 0 {
		allowed := false
		for _, p := range t.IPAllow {
			allowed = allowed || (ip.IsValid() && p.Contains(ip))
		}
		if !allowed {
			return nil, &Denied{http.StatusForbidden, "forbidden", "this token is not allowed from your IP address"}
		}
	}
	return &t, nil
}

// StripQuery removes token parameters from a raw query string.
func StripQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	out := parts[:0]
	for _, p := range parts {
		k, _, _ := strings.Cut(p, "=")
		if uk, err := url.QueryUnescape(k); err == nil {
			k = uk
		}
		if !slices.ContainsFunc(QueryParams, func(q string) bool { return strings.EqualFold(q, k) }) {
			out = append(out, p)
		}
	}
	return strings.Join(out, "&")
}

// IsDenied unwraps a Denied error.
func IsDenied(err error) (*Denied, bool) {
	var d *Denied
	ok := errors.As(err, &d)
	return d, ok
}
