// Package provider holds what differs between upstream APIs (ARCHITECTURE.md §5):
// which REST paths are allowed, how the upstream key is injected, how a
// response maps to a key/egress action, and what a call cost.
package provider

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// TaskKind marks async task endpoints, whose follow-up calls must use the key
// (and egress) that created the task.
type TaskKind int

const (
	TaskNone TaskKind = iota
	TaskCreate
	TaskPoll
)

// Operation is one allowed upstream endpoint.
type Operation struct {
	Provider       string
	Name           string // search, extract, contents, research, research_get, …
	Method         string
	Path           string // upstream path, e.g. /search or /research/abc
	Billable       bool
	RetryAfterSend bool     // safe to resend to another key after the request reached upstream
	Task           TaskKind // async task create/poll
	TaskID         string   // TaskPoll: id taken from the path
	Local          bool     // answered by the gateway (e.g. Tavily GET /usage)
	UsageParam     bool     // Tavily: supports include_usage for exact credits
}

// Class is what an upstream response means for routing.
type Class int

const (
	ClassOK           Class = iota
	ClassClientError        // the request itself is wrong: return as-is, never retry
	ClassKeyInvalid         // 401
	ClassKeyExhausted       // quota or budget used up (Tavily 432/433, Exa 402)
	ClassRateLimited        // 429
	ClassKeyForbidden       // 403 with an API error body
	ClassEdgeBlocked        // 403 HTML from a CDN/WAF: the egress IP is blocked, not the key
	ClassServerError        // 5xx
	ClassUpstreamBusy       // upstream refused for load reasons unrelated to the key (Tavily 434)
)

var classNames = map[Class]string{
	ClassOK: "ok", ClassClientError: "client_error", ClassKeyInvalid: "key_invalid", ClassKeyExhausted: "key_exhausted",
	ClassRateLimited: "rate_limited", ClassKeyForbidden: "key_forbidden", ClassEdgeBlocked: "edge_blocked", ClassServerError: "server_error", ClassUpstreamBusy: "upstream_busy",
}

func (c Class) String() string { return classNames[c] }

// Outcome is the classification of one upstream response.
type Outcome struct {
	Class      Class
	Status     int
	Retry      bool          // another key may succeed
	RetryAfter time.Duration // from Retry-After (0 = absent); the pool clamps it
	Reason     string        // short error text from the body, for logs

	// 429 policy. Without Retry-After the cooldown starts at CooldownBase and
	// doubles up to CooldownMax.
	CooldownBase, CooldownMax time.Duration
	// TeamWide: the cause is shared by every key of the key's team (Exa rate
	// limits, team credits or budget, revoked team access), so the pool applies
	// the outcome to all of them — switching to a sibling key cannot help.
	TeamWide bool
}

// Limit is one documented upstream rate limit consumed by a call.
type Limit struct {
	Bucket string  // e.g. "default", "crawl", "search"
	Count  float64 // allowed requests per Window
	Window time.Duration
}

// Cost of one call. Tavily bills credits, Exa bills dollars (kept in micro-USD
// so a $0.005 search does not round to zero).
type Cost struct {
	Credits   int64
	MicroUSD  int64
	Estimated bool
}

// Quota is a key's balance as reported upstream (Tavily GET /usage).
//
// Used is the key's own usage in the current billing cycle. Remaining is what
// the key can still spend: the tighter of its own limit (when one is set) and
// its account's plan plus pay-as-you-go balance — which the account's other
// keys share. Limit = Used + Remaining, so the three always add up. Nil means
// unknown, or unlimited when upstream reports no limit at all.
type Quota struct {
	Limit, Used, Remaining *int64
	Plan                   string
}

// ErrUnsupported is returned for features a provider does not have (Exa has no balance API).
var ErrUnsupported = errors.New("provider: not supported")

// Provider is implemented by tavily and exa.
type Provider interface {
	ID() string
	BaseURL() string
	// Match maps a REST path (without the /tavily or /exa prefix) to an allowed operation.
	Match(method, path string) (Operation, bool)
	// Authorize sets the upstream credential on an otherwise clean header set.
	Authorize(h http.Header, key string)
	// RewriteRequest removes client credentials from the body and, with
	// preciseBilling, asks upstream for exact usage. injected reports a field
	// the gateway added that RewriteResponse must strip again.
	RewriteRequest(op Operation, body []byte, preciseBilling bool) (out []byte, injected bool, err error)
	RewriteResponse(op Operation, body []byte, injected bool) []byte
	Classify(op Operation, status int, h http.Header, body []byte) Outcome
	ActualCost(op Operation, body []byte) (Cost, bool)
	EstimateCost(op Operation, body []byte) Cost
	// Limits lists the upstream rate limits a call consumes. override is the
	// key's rate_limit setting: it replaces the main endpoint limit (0 = default).
	Limits(op Operation, body []byte, secret string, override float64) []Limit
	// TaskIDFromResponse returns the id of a task created by a TaskCreate call.
	TaskIDFromResponse(op Operation, body []byte) string
	// QueryPreview is the first n characters of what was searched/fetched (§13.2).
	QueryPreview(op Operation, body []byte, n int) string
	// UsageRequest / ParseUsage implement quota sync; ErrUnsupported when absent.
	UsageRequest(ctx context.Context, key string) (*http.Request, error)
	ParseUsage(body []byte) (Quota, error)
}

// Get returns the provider for id with its production base URL.
func Get(id string) (Provider, bool) {
	switch id {
	case "tavily":
		return NewTavily(""), true
	case "exa":
		return NewExa(""), true
	}
	return nil, false
}

// ---- shared helpers -------------------------------------------------------

func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// classifyCommon handles the status codes both providers share; ok=false
// leaves provider-specific codes to the caller.
func classifyCommon(status int, h http.Header, body []byte, reason string) (Outcome, bool) {
	o := Outcome{Status: status, Reason: reason}
	switch {
	case status >= 200 && status < 300:
		o.Class = ClassOK
	case status == http.StatusUnauthorized:
		o.Class, o.Retry = ClassKeyInvalid, true
	case status == http.StatusTooManyRequests:
		o.Class, o.Retry, o.RetryAfter = ClassRateLimited, true, ParseRetryAfter(h.Get("Retry-After"), time.Now())
	case status == http.StatusForbidden:
		if isHTML(h, body) {
			o.Class, o.Retry, o.Reason = ClassEdgeBlocked, true, "blocked by upstream edge (HTML 403)"
		} else {
			o.Class, o.Retry = ClassKeyForbidden, true
		}
	case status >= 500:
		o.Class, o.Retry = ClassServerError, true
	default:
		return o, false
	}
	return o, true
}

func isHTML(h http.Header, body []byte) bool {
	if strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/html") {
		return true
	}
	head := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 256)])))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}

// ParseRetryAfter accepts delta-seconds or an HTTP date; 0 when absent or invalid.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// errorText pulls a short human-readable error out of an upstream body.
func errorText(body []byte, paths ...string) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	for _, p := range paths {
		r := gjson.GetBytes(body, p)
		if !r.Exists() {
			continue
		}
		s := r.String()
		if r.IsObject() || r.IsArray() {
			s = r.Raw
		}
		if s = strings.TrimSpace(s); s != "" {
			return truncateRunes(s, 200)
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// RedactSecret removes a key from text that came from upstream before it is logged.
func RedactSecret(s, key string) string {
	if key == "" || len(key) < 6 {
		return s
	}
	return strings.ReplaceAll(s, key, "[key]")
}

// previewString renders s for the request log: whitespace collapsed, first n characters.
func previewString(s string, n int) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), n)
}

// previewURLs shows the first URL (query and fragment dropped) plus "+N" for the rest.
func previewURLs(r gjson.Result, n int) string {
	var urls []string
	if r.IsArray() {
		for _, u := range r.Array() {
			urls = append(urls, u.String())
		}
	} else if r.Exists() {
		urls = []string{r.String()}
	}
	if len(urls) == 0 {
		return ""
	}
	first := urls[0]
	if u, err := url.Parse(first); err == nil && u.Host != "" {
		u.RawQuery, u.Fragment, u.User = "", "", nil
		first = u.String()
	}
	s := previewString(first, n)
	if len(urls) > 1 {
		s += " +" + strconv.Itoa(len(urls)-1)
	}
	return s
}

func countURLs(r gjson.Result) int {
	switch {
	case r.IsArray():
		return len(r.Array())
	case r.Exists() && r.String() != "":
		return 1
	}
	return 0
}

func ceilDiv(a, b int) int {
	if a <= 0 {
		return 0
	}
	return int(math.Ceil(float64(a) / float64(b)))
}
