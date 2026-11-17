package httpx

import (
	"net/url"
	"strings"
)

// sensitiveQueryKeys are query parameters that may carry gateway tokens or
// upstream keys. Matched case-insensitively after unescaping.
var sensitiveQueryKeys = map[string]bool{
	"tavilyapikey": true,
	"exaapikey":    true,
	"key":          true,
	"api_key":      true,
	"apikey":       true,
	"token":        true,
	"access_token": true,
}

// Redacted is the placeholder written in place of secret values.
const Redacted = "REDACTED"

// RedactQuery masks secret parameter values while preserving parameter order
// and every non-sensitive byte, so logs stay readable but never hold tokens.
func RedactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		k, _, hasVal := strings.Cut(p, "=")
		uk, err := url.QueryUnescape(k)
		if err != nil {
			uk = k
		}
		if sensitiveQueryKeys[strings.ToLower(uk)] {
			if hasVal {
				parts[i] = k + "=" + Redacted
			}
		}
	}
	return strings.Join(parts, "&")
}

// RedactURL renders a request URL path plus redacted query for logging.
func RedactURL(u *url.URL) string {
	s := u.EscapedPath()
	if u.RawQuery != "" {
		s += "?" + RedactQuery(u.RawQuery)
	}
	return s
}
