// Package quota keeps upstream balances fresh (ARCHITECTURE.md §10.2).
//
// It syncs Tavily keys with GET /usage, which upstream limits to 10 calls per
// key per 10 minutes. The syncer therefore runs one sync at a time, leaves at
// least Gap between two syncs of the same key or through the same exit IP,
// backs off on 429, and sends every sync through the key's own egress with the
// key's usual User-Agent — never through another egress or direct (§7.2).
// Exa has no balance API for normal keys; its keys are tracked by the pool's
// local ledger only.
package quota

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
)

// DefaultGap is the minimum time between two syncs of one key, and between
// two syncs through one exit IP: the documented 10 calls per 10 minutes, plus
// a margin.
const DefaultGap = 65 * time.Second

const (
	hotWindow      = 2 * time.Hour  // a key used this recently is "hot"
	usageLag       = time.Hour      // upstream counters lag behind billing (observed 2026-11-10: updated in batches, 6 to 45 minutes late)
	exhaustedEvery = 6 * time.Hour  // an exhausted key may have been reset upstream
	invalidEvery   = 24 * time.Hour // retest of invalid keys (§6.1)
	errorBackoff   = 5 * time.Minute
	requestTimeout = 20 * time.Second
	maxBody        = 1 << 20
)

// rateBackoff is the wait after the 1st, 2nd, 3rd… consecutive 429 of a key.
var rateBackoff = []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute}

// Pool is the part of *pool.Pool the syncer uses.
type Pool interface {
	Views() []pool.View
	SetQuota(id string, q provider.Quota, at time.Time)
	SyncFailed(id string, o provider.Outcome, msg string)
	DeferSync(id string, until time.Time)
	ProbeOK(id string)
}

// prober is a provider that can check a key without a balance API (Exa).
type prober interface {
	ProbeRequest(ctx context.Context, key string) (*http.Request, error)
	ProbeValid(status int, body []byte) (valid, known bool)
}

// Outbound is the part of *outbound.Runtime the syncer uses.
type Outbound interface {
	Client(keyID string) *http.Client
	Usable(egressID string) bool
	ExitIP(egressID string) string
	ReportConnectFailure(egressID string, err error)
	ReportSuccess(egressID string)
	MarkBlocked(egressID string)
}

// Options wires a Syncer.
type Options struct {
	Snapshot  func() *confstore.Snapshot
	Pool      Pool
	Out       Outbound
	Providers map[string]provider.Provider // nil = production endpoints
	Logger    *slog.Logger
	Now       func() time.Time
	Gap       time.Duration // 0 = DefaultGap
	// StartDelay holds back the first sync after start, because the exit-IP
	// gate lives in memory only (0 = Gap, negative = none).
	StartDelay time.Duration
	Tick       time.Duration // how often Run looks for due syncs (0 = 1 s)
}

// Syncer is safe for concurrent use.
type Syncer struct {
	o     Options
	log   *slog.Logger
	gap   time.Duration
	start time.Time
	usage map[string]provider.Provider // providers with a balance API

	mu      sync.Mutex
	gates   map[string]time.Time // exit IP (or egress) → start of its last sync
	wanted  map[string]bool      // manual requests
	probes  []string             // keys to test with a free probe (no balance API)
	hits429 map[string]int       // consecutive 429s per key
	wake    chan struct{}
}

// New returns a Syncer; call Run to start it.
func New(o Options) *Syncer {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Gap <= 0 {
		o.Gap = DefaultGap
	}
	if o.Tick <= 0 {
		o.Tick = time.Second
	}
	if o.Providers == nil {
		o.Providers = map[string]provider.Provider{}
		for _, id := range confstore.Providers {
			if p, ok := provider.Get(id); ok {
				o.Providers[id] = p
			}
		}
	}
	delay := o.StartDelay
	switch {
	case delay == 0:
		delay = o.Gap
	case delay < 0:
		delay = 0
	}
	s := &Syncer{o: o, log: o.Logger, gap: o.Gap, start: o.Now().Add(delay), usage: map[string]provider.Provider{},
		gates: map[string]time.Time{}, wanted: map[string]bool{}, hits429: map[string]int{}, wake: make(chan struct{}, 1)}
	for id, p := range o.Providers {
		if _, err := p.UsageRequest(context.Background(), ""); !errors.Is(err, provider.ErrUnsupported) {
			s.usage[id] = p
		}
	}
	return s
}

// Request queues a sync of the given keys (every syncable key when none are
// given), also when background sync is off. Queued keys go first but still
// wait for the gates. It returns how many keys were queued.
func (s *Syncer) Request(ids ...string) int {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	n := 0
	s.mu.Lock()
	for _, k := range s.o.Snapshot().Keys {
		if s.usage[k.Provider] != nil && k.Enabled && (len(ids) == 0 || want[k.ID]) {
			s.wanted[k.ID] = true
			n++
		}
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return n
}

// Test checks keys on request (the panel's "test" button): keys with a
// balance API are queued for a quota sync, which also proves the key works;
// the others get a free probe soon. It returns how many keys were queued.
func (s *Syncer) Test(ids ...string) int {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var syncIDs []string
	n := 0
	s.mu.Lock()
	for _, k := range s.o.Snapshot().Keys {
		if len(ids) > 0 && !want[k.ID] {
			continue
		}
		switch p := s.o.Providers[k.Provider]; {
		case s.usage[k.Provider] != nil:
			syncIDs = append(syncIDs, k.ID)
		case p != nil:
			if _, ok := p.(prober); ok && !slices.Contains(s.probes, k.ID) {
				s.probes = append(s.probes, k.ID)
				n++
			}
		}
	}
	s.mu.Unlock()
	if len(syncIDs) > 0 {
		n += s.Request(syncIDs...)
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return n
}

// runProbes tests the queued keys, one after another, through each key's own egress.
func (s *Syncer) runProbes(ctx context.Context) {
	for ctx.Err() == nil {
		s.mu.Lock()
		if len(s.probes) == 0 {
			s.mu.Unlock()
			return
		}
		id := s.probes[0]
		s.probes = s.probes[1:]
		s.mu.Unlock()
		k, ok := s.o.Snapshot().KeyByID(id)
		if !ok {
			continue
		}
		s.probe(ctx, k)
	}
}

func (s *Syncer) probe(ctx context.Context, k confstore.Key) {
	pr, _ := s.o.Providers[k.Provider].(prober)
	fail := func(o provider.Outcome, msg string) {
		s.o.Pool.SyncFailed(k.ID, o, msg)
		s.log.Warn("key test failed", "provider", k.Provider, "key", k.DisplayName(), "err", msg)
	}
	if !k.Egress.Enabled || !s.o.Out.Usable(k.Egress.ID) {
		fail(provider.Outcome{}, "the key's egress is not usable; not tested (never sent another way)")
		return
	}
	client := s.o.Out.Client(k.ID)
	if pr == nil || client == nil {
		fail(provider.Outcome{}, "no upstream connection for this key yet")
		return
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := pr.ProbeRequest(rctx, k.Secret)
	if err != nil {
		fail(provider.Outcome{}, "build request: "+cleanErr(err, k.Secret))
		return
	}
	req.Header.Set("User-Agent", s.o.Snapshot().Config.Upstream.UserAgentFor(k.Provider))
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			fail(provider.Outcome{Class: provider.ClassServerError}, "request failed: "+cleanErr(err, k.Secret))
		}
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	s.o.Out.ReportSuccess(k.Egress.ID)
	switch valid, known := pr.ProbeValid(resp.StatusCode, body); {
	case valid:
		s.o.Pool.ProbeOK(k.ID)
		s.log.Info("key test passed", "provider", k.Provider, "key", k.DisplayName())
	case known:
		fail(provider.Outcome{Class: provider.ClassKeyInvalid, Status: resp.StatusCode}, "invalid key (HTTP "+strconv.Itoa(resp.StatusCode)+")")
	default:
		fail(provider.Outcome{}, "test inconclusive: HTTP "+strconv.Itoa(resp.StatusCode))
	}
}

// Run syncs due keys, one at a time, until ctx is done.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(s.o.Tick)
	defer t.Stop()
	for {
		s.runProbes(ctx)
		for ctx.Err() == nil {
			if _, ran := s.Step(ctx); !ran {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

type job struct {
	key  confstore.Key
	p    provider.Provider
	gate string
	prio int
	last time.Time
}

// Step runs the most urgent due sync, if any, and returns its key ID.
func (s *Syncer) Step(ctx context.Context) (string, bool) {
	now := s.o.Now()
	if now.Before(s.start) {
		return "", false
	}
	snap := s.o.Snapshot()
	views := map[string]pool.View{}
	for _, v := range s.o.Pool.Views() {
		views[v.ID] = v
	}
	s.mu.Lock()
	j, ok := s.pick(snap, views, now)
	if ok {
		s.gates[j.gate] = now // reserve before sending
		delete(s.wanted, j.key.ID)
	}
	s.mu.Unlock()
	if !ok {
		return "", false
	}
	s.o.Pool.DeferSync(j.key.ID, now.Add(s.gap)) // persisted: holds across restarts
	s.sync(ctx, snap, j, now)
	return j.key.ID, true
}

// pick chooses the most urgent sync the gates allow now. Callers hold s.mu.
func (s *Syncer) pick(snap *confstore.Snapshot, views map[string]pool.View, now time.Time) (job, bool) {
	var best job
	found := false
	for _, k := range snap.Keys {
		p := s.usage[k.Provider]
		v, known := views[k.ID]
		if p == nil || !known || !k.Enabled || !k.Egress.Enabled || !s.o.Out.Usable(k.Egress.ID) {
			continue // a sync only ever goes out through the key's own, working egress
		}
		if now.Before(v.NextSyncAt) {
			continue
		}
		gate := s.gateOf(k)
		if last, ok := s.gates[gate]; ok && now.Sub(last) < s.gap {
			continue
		}
		prio := priority(v, now, snap.Config.Quota, s.wanted[k.ID])
		if prio < 0 {
			continue
		}
		j := job{key: k, p: p, gate: gate, prio: prio, last: v.LastSyncedAt}
		if !found || j.prio < best.prio || (j.prio == best.prio && j.last.Before(best.last)) {
			best, found = j, true
		}
	}
	return best, found
}

// gateOf is the exit IP a sync of k leaves from; while the egress's IP is not
// yet known, the egress itself stands in for it.
func (s *Syncer) gateOf(k confstore.Key) string {
	if ip := s.o.Out.ExitIP(k.Egress.ID); ip != "" {
		return "ip:" + ip
	}
	return "egress:" + k.Egress.ID
}

// priority orders due syncs, lower first; -1 means not due.
func priority(v pool.View, now time.Time, cfg confstore.QuotaConfig, wanted bool) int {
	since := now.Sub(v.LastSyncedAt)
	hot := time.Duration(cfg.HotIntervalMinutes) * time.Minute
	cold := time.Duration(cfg.ColdIntervalHours) * time.Hour
	switch {
	case wanted:
		return 0
	case !cfg.Sync:
		return -1
	case v.LastSyncedAt.IsZero() || v.NeedSync:
		return 1
	case v.Status == pool.StatusInvalid:
		if since >= invalidEvery {
			return 3
		}
		return -1
	case v.Status == pool.StatusExhausted:
		if since >= exhaustedEvery {
			return 3
		}
		return -1
	case hot > 0 && since >= hot && now.Sub(v.LastUsedAt) < hotWindow && v.LastUsedAt.After(v.LastSyncedAt.Add(-usageLag)):
		return 2 // used recently, possibly after what the last sync could see
	case cold > 0 && since >= cold:
		return 4
	}
	return -1
}

func usageOp(providerID string) provider.Operation {
	return provider.Operation{Provider: providerID, Name: "usage", Method: http.MethodGet, Path: "/usage", Local: true}
}

func (s *Syncer) sync(ctx context.Context, snap *confstore.Snapshot, j job, now time.Time) {
	k := j.key
	fail := func(o provider.Outcome, msg string, retry time.Duration) {
		s.o.Pool.SyncFailed(k.ID, o, msg)
		s.o.Pool.DeferSync(k.ID, now.Add(max(retry, s.gap)))
		s.log.Warn("quota sync failed", "provider", k.Provider, "key", k.DisplayName(), "egress", k.Egress.Label(), "err", msg)
	}
	client := s.o.Out.Client(k.ID)
	if client == nil {
		fail(provider.Outcome{}, "no upstream connection for this key yet", s.gap)
		return
	}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var wrote atomic.Bool
	rctx = httptrace.WithClientTrace(rctx, &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			wrote.Store(true)
		}
	}})
	req, err := j.p.UsageRequest(rctx, k.Secret)
	if err != nil {
		fail(provider.Outcome{}, "build request: "+cleanErr(err, k.Secret), errorBackoff)
		return
	}
	req.Header.Set("User-Agent", snap.Config.Upstream.UserAgentFor(k.Provider))

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down: not the key's or the egress's fault
		}
		if !wrote.Load() { // never reached upstream: the egress failed (§7.4)
			s.o.Out.ReportConnectFailure(k.Egress.ID, err)
		}
		fail(provider.Outcome{Class: provider.ClassServerError}, "request failed: "+cleanErr(err, k.Secret), errorBackoff)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		fail(provider.Outcome{Class: provider.ClassServerError}, "reading the response: "+cleanErr(err, k.Secret), errorBackoff)
		return
	}
	s.o.Out.ReportSuccess(k.Egress.ID)

	o := j.p.Classify(usageOp(k.Provider), resp.StatusCode, resp.Header, body)
	o.Reason = provider.RedactSecret(o.Reason, k.Secret)
	switch o.Class {
	case provider.ClassOK:
		q, err := j.p.ParseUsage(body)
		if err != nil {
			fail(o, err.Error(), errorBackoff)
			return
		}
		s.mu.Lock()
		delete(s.hits429, k.ID)
		s.mu.Unlock()
		s.o.Pool.SetQuota(k.ID, q, now)
		s.log.Debug("quota synced", "provider", k.Provider, "key", k.DisplayName(), "plan", q.Plan)
	case provider.ClassRateLimited:
		s.mu.Lock()
		s.hits429[k.ID]++
		n := s.hits429[k.ID]
		s.mu.Unlock()
		fail(o, "rate limited (429)", max(rateBackoff[min(n, len(rateBackoff))-1], o.RetryAfter))
	case provider.ClassKeyInvalid:
		fail(o, "invalid key (HTTP "+strconv.Itoa(resp.StatusCode)+")", invalidEvery)
	case provider.ClassEdgeBlocked:
		s.o.Out.MarkBlocked(k.Egress.ID)
		fail(o, "blocked by the upstream edge (HTML 403)", errorBackoff)
	default:
		msg := "HTTP " + strconv.Itoa(resp.StatusCode)
		if o.Reason != "" {
			msg += ": " + o.Reason
		}
		fail(o, msg, errorBackoff)
	}
}

var userinfoRe = regexp.MustCompile(`//[^/@\s]+@`)

// cleanErr keeps errors safe to log and show: no key, no proxy credentials.
func cleanErr(err error, secret string) string {
	s := userinfoRe.ReplaceAllString(provider.RedactSecret(err.Error(), secret), "//***@")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}
