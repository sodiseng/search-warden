// Package outbound owns every connection to upstream (ARCHITECTURE.md §7.3–7.5):
// one http.Transport per key (keys never share TCP/TLS connections), each
// pinned to the key's egress with no system-proxy fallback, plus egress health
// (active probes, passive connect-failure counting, quarantine with backoff,
// edge blocks) and exit-IP detection.
package outbound

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// Health states persisted in egress_state.health.
const (
	HealthUnknown     = "unknown"
	HealthHealthy     = "healthy"
	HealthQuarantined = "quarantined"
	HealthBlocked     = "blocked"
)

const (
	failThreshold   = 3
	backoffMin      = 30 * time.Second
	backoffMax      = 10 * time.Minute
	blockDuration   = 6 * time.Hour
	exitIPRefresh   = 6 * time.Hour
	probeTimeout    = 10 * time.Second
	probeConcurrent = 8
)

// StateStore is the subset of *store.Store used here.
type StateStore interface {
	ListEgressStates(ctx context.Context) (map[string]store.EgressState, error)
	UpsertEgressStates(ctx context.Context, states []store.EgressState) error
	DeleteEgressStatesExcept(ctx context.Context, keep []string) (int64, error)
}

// Options configures a Runtime.
type Options struct {
	Store        StateStore
	Logger       *slog.Logger
	Now          func() time.Time
	ProbeTargets []string // URLs fetched (no credentials) to check an egress; default: both upstream APIs
	IPEchoURLs   []string // exit-IP services; default ipify, then Cloudflare trace
	// DialContext dials TCP for direct egress and for reaching proxies. Tests
	// hook it to prove that a proxied key never dials upstream directly.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
}

type egressState struct {
	id, url, label string // url "" = direct
	enabled        bool
	health         string
	failStreak     int
	until          time.Time // quarantine or block end
	backoff        time.Duration
	nextProbe      time.Time
	probing        bool
	latencyMS      int
	exitIP         string
	exitCountry    string
	lastChecked    time.Time
	lastIPCheck    time.Time
	lastError      string
	epoch          uint64
	probe          *http.Client
	dirty          bool
}

type keyClient struct {
	egressID string
	client   *http.Client
	tr       *http.Transport
}

// Runtime is safe for concurrent use.
type Runtime struct {
	opts      Options
	log       *slog.Logger
	mu        sync.Mutex
	eg        map[string]*egressState
	keys      map[string]*keyClient
	interval  time.Duration
	persisted map[string]store.EgressState
	removed   bool
	wake      chan struct{}
}

// New creates a Runtime; call Load, then Apply, then run Run in a goroutine.
func New(opts Options) *Runtime {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if len(opts.ProbeTargets) == 0 {
		opts.ProbeTargets = []string{"https://api.tavily.com/", "https://api.exa.ai/"}
	}
	if len(opts.IPEchoURLs) == 0 {
		opts.IPEchoURLs = []string{"https://api.ipify.org", "https://www.cloudflare.com/cdn-cgi/trace", "https://icanhazip.com"}
	}
	if opts.DialContext == nil {
		d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		opts.DialContext = d.DialContext
	}
	return &Runtime{opts: opts, log: opts.Logger, eg: map[string]*egressState{}, keys: map[string]*keyClient{},
		interval: time.Minute, persisted: map[string]store.EgressState{}, wake: make(chan struct{}, 1)}
}

// newTransport builds a transport that only ever uses the given proxy (nil =
// direct). It never consults HTTP(S)_PROXY: a system proxy must not silently
// re-route a key (§7.3). Socks5 targets are sent as hostnames, so DNS is
// resolved by the proxy (Go treats socks5 like socks5h).
func (r *Runtime) newTransport(proxy string) (*http.Transport, error) {
	t := &http.Transport{
		DialContext:           r.opts.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
		ExpectContinueTimeout: time.Second,
	}
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		t.Proxy = http.ProxyURL(u)
	}
	return t, nil
}

func newClient(t *http.Transport) *http.Client {
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Load restores persisted egress health (exit IP, quarantine) at startup.
func (r *Runtime) Load(ctx context.Context) error {
	if r.opts.Store == nil {
		return nil
	}
	m, err := r.opts.Store.ListEgressStates(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.persisted = m
	r.mu.Unlock()
	return nil
}

// Apply syncs egresses and per-key clients with the configuration.
func (r *Runtime) Apply(snap *confstore.Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := snap.Config.Egress.HealthIntervalSeconds; s > 0 && r.interval != time.Duration(s)*time.Second {
		r.interval = time.Duration(s) * time.Second
		// A shorter interval applies now, not after the old one runs out.
		soon := r.opts.Now().Add(r.interval)
		for _, st := range r.eg {
			if st.health == HealthHealthy && st.nextProbe.After(soon) {
				st.nextProbe = soon
			}
		}
	}
	want := map[string]egress.Resolved{egress.DirectID: {ID: egress.DirectID, Enabled: true}}
	for _, n := range snap.Egresses.Nodes() {
		res, _ := snap.Egresses.Resolve(n.Name)
		want[res.ID] = res
	}
	for _, k := range snap.Keys {
		want[k.Egress.ID] = k.Egress
	}
	for id, res := range want {
		st := r.eg[id]
		if st == nil {
			st = &egressState{id: id, health: HealthUnknown}
			if ps, ok := r.persisted[id]; ok {
				st.health, st.exitIP, st.exitCountry, st.latencyMS = ps.Health, ps.ExitIP, ps.ExitCountry, ps.LatencyMS
				st.failStreak, st.until, st.lastChecked, st.lastError = ps.FailStreak, ps.QuarantinedUntil, ps.LastCheckedAt, ps.LastError
				if st.health == "" {
					st.health = HealthUnknown
				}
			}
			r.eg[id] = st
			st.dirty = true
		}
		if st.url != res.URL || st.probe == nil {
			tr, err := r.newTransport(res.URL)
			if err != nil {
				r.log.Error("egress transport", "egress", res.Label(), "err", err)
				continue
			}
			if st.probe != nil {
				st.probe.CloseIdleConnections()
			}
			st.probe = newClient(tr)
		}
		st.url, st.label, st.enabled = res.URL, res.Label(), res.Enabled
	}
	for id, st := range r.eg {
		if _, ok := want[id]; !ok {
			if st.probe != nil {
				st.probe.CloseIdleConnections()
			}
			delete(r.eg, id)
			r.removed = true
		}
	}
	seen := map[string]bool{}
	for _, k := range snap.Keys {
		seen[k.ID] = true
		kc := r.keys[k.ID]
		if kc != nil && kc.egressID == k.Egress.ID {
			continue
		}
		tr, err := r.newTransport(k.Egress.URL)
		if err != nil {
			r.log.Error("key transport", "key", k.DisplayName(), "err", err)
			continue
		}
		if kc != nil {
			kc.tr.CloseIdleConnections() // the key moved to another egress
		}
		r.keys[k.ID] = &keyClient{egressID: k.Egress.ID, client: newClient(tr), tr: tr}
	}
	for id, kc := range r.keys {
		if !seen[id] {
			kc.tr.CloseIdleConnections()
			delete(r.keys, id)
		}
	}
	r.poke()
}

func (r *Runtime) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// ProbeNow schedules an immediate health check (and exit-IP check) of the
// given egresses, or of every egress when none are given — the panel's "test"
// button. A blocked egress is retested too: a passing probe clears the block.
func (r *Runtime) ProbeNow(ids ...string) {
	r.mu.Lock()
	now := r.opts.Now()
	for id, st := range r.eg {
		if len(ids) == 0 || slices.Contains(ids, id) {
			st.nextProbe = now
			st.lastIPCheck = time.Time{}
		}
	}
	r.mu.Unlock()
	r.poke()
}

// Client returns the dedicated HTTP client of a key (nil if unknown).
func (r *Runtime) Client(keyID string) *http.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if kc := r.keys[keyID]; kc != nil {
		return kc.client
	}
	return nil
}

// Usable reports whether keys bound to the egress may be used now.
func (r *Runtime) Usable(egressID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.eg[egressID]
	if st == nil || !st.enabled {
		return false
	}
	// Quarantined/blocked egresses stay out until a probe confirms recovery;
	// their keys pause instead of falling back to direct (§7.2).
	return st.health != HealthQuarantined && st.health != HealthBlocked
}

// ExitIP is the last detected public IP of the egress ("" if unknown).
func (r *Runtime) ExitIP(egressID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.eg[egressID]; st != nil {
		return st.exitIP
	}
	return ""
}

var userinfoRe = regexp.MustCompile(`//[^/@\s]+@`)

func sanitize(err error) string {
	if err == nil {
		return ""
	}
	s := userinfoRe.ReplaceAllString(err.Error(), "//***@")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// ReportConnectFailure counts a failure to reach upstream through the egress
// (dial, proxy handshake or TLS — before the request was sent). Upstream 5xx
// and timeouts after sending are NOT egress failures (fixes tavily-lb, §7.4).
func (r *Runtime) ReportConnectFailure(egressID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.eg[egressID]
	if st == nil {
		return
	}
	st.failStreak++
	st.lastError, st.dirty = sanitize(err), true
	if st.failStreak >= failThreshold && st.health != HealthQuarantined {
		r.quarantine(st, "connection failures")
	}
}

// ReportSuccess notes a working connection through the egress.
func (r *Runtime) ReportSuccess(egressID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.eg[egressID]; st != nil && (st.failStreak != 0 || st.health == HealthUnknown) {
		st.failStreak = 0
		if st.health == HealthUnknown {
			st.health = HealthHealthy
		}
		st.dirty = true
	}
}

// MarkBlocked records an upstream edge block (HTML 403): the egress IP is
// blocked, so its keys pause; it is retried after blockDuration.
func (r *Runtime) MarkBlocked(egressID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.eg[egressID]
	if st == nil || st.health == HealthBlocked {
		return
	}
	now := r.opts.Now()
	st.health, st.until, st.nextProbe = HealthBlocked, now.Add(blockDuration), now.Add(blockDuration)
	st.lastError, st.dirty = "blocked by upstream edge (HTML 403)", true
	st.epoch++
	r.log.Warn("egress blocked by upstream", "egress", st.label, "exit_ip", st.exitIP, "retry_at", st.until)
}

func (r *Runtime) quarantine(st *egressState, why string) {
	now := r.opts.Now()
	if st.backoff == 0 {
		st.backoff = backoffMin
	} else {
		st.backoff = min(st.backoff*2, backoffMax)
	}
	st.health, st.until, st.nextProbe = HealthQuarantined, now.Add(st.backoff), now.Add(st.backoff)
	st.epoch++ // older in-flight probes must not overwrite this
	st.dirty = true
	r.log.Warn("egress quarantined", "egress", st.label, "reason", why, "retry_in", st.backoff, "last_error", st.lastError)
}

// ---- views & persistence --------------------------------------------------

// View is an egress's runtime state for the panel.
type View struct {
	ID, Label, Health, ExitIP, ExitCountry, LastError string
	Enabled                                           bool
	LatencyMS, FailStreak                             int
	Until, LastChecked                                time.Time
}

// Views lists egresses, direct first then by label.
func (r *Runtime) Views() []View {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]View, 0, len(r.eg))
	for _, st := range r.eg {
		out = append(out, View{ID: st.id, Label: st.label, Health: st.health, ExitIP: st.exitIP, ExitCountry: st.exitCountry,
			LastError: st.lastError, Enabled: st.enabled, LatencyMS: st.latencyMS, FailStreak: st.failStreak, Until: st.until, LastChecked: st.lastChecked})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].ID == egress.DirectID) != (out[j].ID == egress.DirectID) {
			return out[i].ID == egress.DirectID
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// Flush persists changed egress states and drops states of removed egresses.
func (r *Runtime) Flush(ctx context.Context) error {
	if r.opts.Store == nil {
		return nil
	}
	r.mu.Lock()
	var batch []store.EgressState
	keep := make([]string, 0, len(r.eg))
	for _, st := range r.eg {
		keep = append(keep, st.id)
		if st.dirty {
			st.dirty = false
			batch = append(batch, store.EgressState{EgressID: st.id, ExitIP: st.exitIP, ExitCountry: st.exitCountry, Health: st.health,
				LatencyMS: st.latencyMS, FailStreak: st.failStreak, QuarantinedUntil: st.until, LastCheckedAt: st.lastChecked, LastError: st.lastError})
		}
	}
	removed := r.removed
	r.removed = false
	r.mu.Unlock()
	if err := r.opts.Store.UpsertEgressStates(ctx, batch); err != nil {
		return fmt.Errorf("egress flush: %w", err)
	}
	if removed {
		if _, err := r.opts.Store.DeleteEgressStatesExcept(ctx, keep); err != nil {
			return fmt.Errorf("egress cleanup: %w", err)
		}
	}
	return nil
}
