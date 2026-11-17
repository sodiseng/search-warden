// Package metrics exposes Prometheus metrics in the text exposition format,
// without a client library: a few counters fed by the request log, and gauges
// read from the key pool, egress runtime and log writer at scrape time.
//
// The endpoint is off unless WARDEN_METRICS_TOKEN is set; scrapers then send it
// as a Bearer token (metrics show traffic shape, keys' states and labels).
package metrics

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// Latency buckets in seconds (searches take ~1–3 s; research polls longer).
var buckets = []float64{0.25, 0.5, 1, 2, 5, 10, 30, 60, 180}

type reqKey struct{ provider, surface, class string }

type hist struct {
	counts []uint64 // per bucket, cumulative at render time
	sum    float64
	n      uint64
}

// Recorder wraps the request-log recorder and counts what passes through.
type Recorder struct {
	next interface {
		Record(l store.RequestLog, attempts []store.Attempt)
	}
	mu       sync.Mutex
	requests map[reqKey]uint64
	latency  map[string]*hist // by provider
	credits  map[string]int64 // by provider
	microUSD map[string]int64
	retries  map[string]uint64
	errors   map[[2]string]uint64 // provider, error kind
}

// NewRecorder counts requests and passes them on to next.
func NewRecorder(next interface {
	Record(l store.RequestLog, attempts []store.Attempt)
}) *Recorder {
	return &Recorder{next: next, requests: map[reqKey]uint64{}, latency: map[string]*hist{}, credits: map[string]int64{},
		microUSD: map[string]int64{}, retries: map[string]uint64{}, errors: map[[2]string]uint64{}}
}

func statusClass(status int) string {
	switch {
	case status == 0:
		return "none"
	case status < 400:
		return "2xx"
	case status == 429:
		return "429"
	case status < 500:
		return "4xx"
	}
	return "5xx"
}

// Record implements relay.Recorder.
func (m *Recorder) Record(l store.RequestLog, attempts []store.Attempt) {
	m.mu.Lock()
	m.requests[reqKey{l.Provider, l.Surface, statusClass(l.Status)}]++
	h := m.latency[l.Provider]
	if h == nil {
		h = &hist{counts: make([]uint64, len(buckets))}
		m.latency[l.Provider] = h
	}
	sec := float64(l.LatencyMS) / 1000
	for i, b := range buckets {
		if sec <= b {
			h.counts[i]++
			break
		}
	}
	h.sum += sec
	h.n++
	m.credits[l.Provider] += l.CostCredits
	m.microUSD[l.Provider] += l.CostMicroUSD
	if l.Attempts > 1 {
		m.retries[l.Provider] += uint64(l.Attempts - 1)
	}
	if l.ErrorKind != "" {
		m.errors[[2]string{l.Provider, l.ErrorKind}]++
	}
	m.mu.Unlock()
	if m.next != nil {
		m.next.Record(l, attempts)
	}
}

// Sources are read at scrape time; any may be nil.
type Sources struct {
	Keys       func() []pool.View
	Egresses   func() []outbound.View
	LogDropped func() int64
	ConfigOK   func() bool
	Version    string
}

// Handler serves /metrics when token is non-empty, 404 otherwise.
func Handler(token string, m *Recorder, src Sources) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.NotFound(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		m.write(w, src)
	})
}

func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func labels(kv ...string) string {
	parts := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, kv[i]+`="`+esc(kv[i+1])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func header(w io.Writer, name, typ, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func sortedKeys[K comparable, V any](m map[K]V, less func(a, b K) bool) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	return out
}

func (m *Recorder) write(w io.Writer, src Sources) {
	m.mu.Lock()
	header(w, "warden_requests_total", "counter", "Requests to upstream by provider, surface (rest|mcp) and status class.")
	for _, k := range sortedKeys(m.requests, func(a, b reqKey) bool { return a.provider+a.surface+a.class < b.provider+b.surface+b.class }) {
		fmt.Fprintf(w, "warden_requests_total%s %d\n", labels("provider", k.provider, "surface", k.surface, "status", k.class), m.requests[k])
	}
	header(w, "warden_request_duration_seconds", "histogram", "Total time of a request, retries included.")
	for _, p := range sortedKeys(m.latency, func(a, b string) bool { return a < b }) {
		h := m.latency[p]
		var cum uint64
		for i, b := range buckets {
			cum += h.counts[i]
			fmt.Fprintf(w, "warden_request_duration_seconds_bucket%s %d\n", labels("provider", p, "le", num(b)), cum)
		}
		fmt.Fprintf(w, "warden_request_duration_seconds_bucket%s %d\n", labels("provider", p, "le", "+Inf"), h.n)
		fmt.Fprintf(w, "warden_request_duration_seconds_sum%s %s\n", labels("provider", p), num(h.sum))
		fmt.Fprintf(w, "warden_request_duration_seconds_count%s %d\n", labels("provider", p), h.n)
	}
	header(w, "warden_upstream_credits_total", "counter", "Tavily credits spent.")
	for _, p := range sortedKeys(m.credits, func(a, b string) bool { return a < b }) {
		if m.credits[p] > 0 {
			fmt.Fprintf(w, "warden_upstream_credits_total%s %d\n", labels("provider", p), m.credits[p])
		}
	}
	header(w, "warden_upstream_usd_total", "counter", "Exa dollars spent.")
	for _, p := range sortedKeys(m.microUSD, func(a, b string) bool { return a < b }) {
		if m.microUSD[p] > 0 {
			fmt.Fprintf(w, "warden_upstream_usd_total%s %s\n", labels("provider", p), num(float64(m.microUSD[p])/1e6))
		}
	}
	header(w, "warden_retries_total", "counter", "Extra upstream attempts (another key after a failure).")
	for _, p := range sortedKeys(m.retries, func(a, b string) bool { return a < b }) {
		fmt.Fprintf(w, "warden_retries_total%s %d\n", labels("provider", p), m.retries[p])
	}
	header(w, "warden_request_errors_total", "counter", "Failed requests by kind (rate_limited, no_keys, token_limit, key_invalid, ...).")
	for _, k := range sortedKeys(m.errors, func(a, b [2]string) bool { return a[0]+a[1] < b[0]+b[1] }) {
		fmt.Fprintf(w, "warden_request_errors_total%s %d\n", labels("provider", k[0], "kind", k[1]), m.errors[k])
	}
	m.mu.Unlock()

	if src.Keys != nil {
		header(w, "warden_keys", "gauge", "Upstream keys by provider and status.")
		count := map[[2]string]int{}
		header2 := "warden_key_remaining"
		var remaining []string
		for _, v := range src.Keys() {
			count[[2]string{v.Provider, string(v.Status)}]++
			if v.Remaining != nil {
				val := float64(*v.Remaining)
				if v.Unit == "micro_usd" {
					val /= 1e6
				}
				remaining = append(remaining, fmt.Sprintf("%s%s %s", header2, labels("provider", v.Provider, "key", v.ID, "label", v.Label, "unit", unitName(v.Unit)), num(val)))
			}
		}
		for _, k := range sortedKeys(count, func(a, b [2]string) bool { return a[0]+a[1] < b[0]+b[1] }) {
			fmt.Fprintf(w, "warden_keys%s %d\n", labels("provider", k[0], "status", k[1]), count[k])
		}
		header(w, header2, "gauge", "Balance left per key (credits for Tavily, dollars for Exa; Exa free keys are estimates).")
		for _, line := range remaining {
			fmt.Fprintln(w, line)
		}
	}
	if src.Egresses != nil {
		header(w, "warden_egress_up", "gauge", "1 when the egress may be used (healthy or not yet checked), 0 when quarantined or blocked.")
		for _, e := range src.Egresses() {
			if !e.Enabled {
				continue
			}
			up := 1
			if e.Health == outbound.HealthQuarantined || e.Health == outbound.HealthBlocked {
				up = 0
			}
			fmt.Fprintf(w, "warden_egress_up%s %d\n", labels("egress", e.Label, "health", e.Health), up)
		}
	}
	if src.LogDropped != nil {
		header(w, "warden_log_dropped_total", "counter", "Request log entries dropped because the write queue was full.")
		fmt.Fprintf(w, "warden_log_dropped_total %d\n", src.LogDropped())
	}
	if src.ConfigOK != nil {
		header(w, "warden_config_healthy", "gauge", "0 when a config file has an edit that was rejected (the last good config is in use).")
		ok := 0
		if src.ConfigOK() {
			ok = 1
		}
		fmt.Fprintf(w, "warden_config_healthy %d\n", ok)
	}
	header(w, "warden_build_info", "gauge", "Build information.")
	fmt.Fprintf(w, "warden_build_info%s 1\n", labels("version", src.Version))
}

func unitName(u string) string {
	if u == "micro_usd" {
		return "usd"
	}
	return u
}
