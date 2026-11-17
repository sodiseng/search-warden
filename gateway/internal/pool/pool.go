// Package pool owns the runtime state of upstream keys and picks a key for
// each upstream attempt (ARCHITECTURE.md §6).
//
// State lives in memory (authoritative) and is flushed to the store every few
// seconds, so the request path never writes to the database (fixing the
// per-request UPDATE storms seen in hikari).
package pool

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// Status of a key. "disabled" and "egress_down" are derived, never stored.
type Status string

const (
	StatusActive     Status = "active"
	StatusCooldown   Status = "cooldown"
	StatusExhausted  Status = "exhausted"
	StatusInvalid    Status = "invalid"
	StatusDisabled   Status = "disabled"
	StatusEgressDown Status = "egress_down"
)

// Tunables (§5.2, §6.1).
const (
	cooldownDefault   = 60 * time.Second
	cooldownMax       = 5 * time.Minute
	cooldownForbidden = 2 * time.Minute
	serverErrorTrip   = 3.0 // fail score that trips a cooldown
	cooldownServerErr = 60 * time.Second
	affinityTopN      = 3
)

// Store is the subset of *store.Store the pool needs.
type Store interface {
	ListKeyStates(ctx context.Context, provider string) ([]store.KeyState, error)
	UpsertKeyStates(ctx context.Context, states []store.KeyState) error
	MarkKeysRemoved(ctx context.Context, keyIDs []string, at time.Time) error
	RestoreKeys(ctx context.Context, keyIDs []string) error
}

// Options configures a Pool.
type Options struct {
	Store        Store
	EgressUsable func(egressID string) bool // nil = every egress is usable
	Logger       *slog.Logger
	Now          func() time.Time
	Rand         *rand.Rand // jitter source; nil = time-seeded
}

type keyState struct {
	cfg        confstore.Key
	status     Status
	until      time.Time
	reason     string
	failScore  float64
	hits429    int
	lastUsed   time.Time
	inFlight   int
	limit      *int64 // Tavily: credits limit from /usage; Exa: budget in micro-USD
	used       *int64 // units used this period (Tavily: upstream + local; Exa: local ledger)
	unit       string // "credits" | "micro_usd"
	periodEnd  time.Time
	lastSynced time.Time
	removedAt  time.Time
	limitSoft  bool      // limit is an estimate (Exa free grant): shown and ranked, never enforced
	plan       string    // plan name reported upstream (Tavily current_plan)
	syncErr    string    // last quota sync error ("" = the last sync succeeded)
	nextSync   time.Time // earliest next quota sync (gap or backoff; persisted)
	needSync   bool      // a new period began: sync as soon as the gates allow
	cw         float64   // smooth weighted round-robin: current weight
	limiter    *rate.Limiter
	dirty      bool
}

// Pool is safe for concurrent use.
type Pool struct {
	mu        sync.Mutex
	opts      Options
	log       *slog.Logger
	rnd       *rand.Rand
	keys      map[string]*keyState
	order     []string // config order (tavily then exa, file order)
	routing   confstore.RoutingConfig
	persisted map[string]store.KeyState
	removed   []string
	restored  []string
	rr        map[string]uint64
	buckets   map[string]*rate.Limiter // upstream limits, by scope and bucket name
}

// New returns an empty pool; call Load then Apply.
func New(opts Options) *Pool {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	rnd := opts.Rand
	if rnd == nil {
		rnd = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x5ead))
	}
	return &Pool{opts: opts, log: opts.Logger, rnd: rnd, keys: map[string]*keyState{}, persisted: map[string]store.KeyState{}, rr: map[string]uint64{}, buckets: map[string]*rate.Limiter{}}
}

// Load reads persisted key states (call once at startup, before the first Apply).
func (p *Pool) Load(ctx context.Context) error {
	if p.opts.Store == nil {
		return nil
	}
	states, err := p.opts.Store.ListKeyStates(ctx, "")
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range states {
		p.persisted[s.KeyID] = s
	}
	return nil
}

func unitFor(provider string) string {
	if provider == "exa" {
		return "micro_usd"
	}
	return "credits"
}

// nextReset is the first resetDay-of-month 00:00 UTC strictly after t.
func nextReset(t time.Time, resetDay int) time.Time {
	t = t.UTC()
	cand := time.Date(t.Year(), t.Month(), resetDay, 0, 0, 0, 0, time.UTC)
	if !cand.After(t) {
		cand = cand.AddDate(0, 1, 0)
	}
	return cand
}

// Apply syncs the pool with a config snapshot: new keys appear (restoring any
// persisted state), removed keys disappear (their state is kept in the store
// for 30 days), and edited keys keep their runtime state.
func (p *Pool) Apply(snap *confstore.Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	p.routing = snap.Config.Routing
	seen := map[string]bool{}
	p.order = p.order[:0]
	for _, k := range snap.Keys {
		seen[k.ID] = true
		p.order = append(p.order, k.ID)
		st := p.keys[k.ID]
		if st == nil {
			st = &keyState{status: StatusActive, unit: unitFor(k.Provider), periodEnd: nextReset(now, k.ResetDay)}
			if ps, ok := p.persisted[k.ID]; ok {
				restoreState(st, ps)
				if !ps.RemovedAt.IsZero() {
					p.restored = append(p.restored, k.ID)
				}
				delete(p.persisted, k.ID)
			}
			p.keys[k.ID] = st
			st.dirty = true
		}
		if st.cfg.ResetDay != k.ResetDay && st.cfg.ID != "" {
			st.periodEnd = nextReset(now, k.ResetDay)
		}
		if weightOf(st.cfg) != weightOf(k) {
			st.cw = 0 // a new weight starts a fresh rotation
		}
		st.cfg = k
		if k.Provider == "exa" {
			st.limit, st.limitSoft = exaLimit(k, snap.Config.Quota.ExaFreeMonthlyUSD)
		}
		st.limiter = limiterFor(p.routing.PerKeyRPM, st.limiter)
	}
	for id, st := range p.keys {
		if !seen[id] {
			st.removedAt = now
			p.removed = append(p.removed, id)
			delete(p.keys, id)
		}
	}
	scopes := map[string]bool{}
	for _, st := range p.keys {
		scopes[scopeOf(st.cfg)] = true
	}
	for id := range p.buckets {
		if scope, _, _ := strings.Cut(id, "|"); !scopes[scope] {
			delete(p.buckets, id)
		}
	}
}

// exaLimit is an Exa key's spending limit in micro-USD. Exa has no balance API
// for normal keys, so a configured budget is the (enforced) limit. A free key
// without one gets the monthly free grant as an estimate: shown and used for
// ranking, never enforced — a team may hold more (a one-time bonus, purchased
// credits), and upstream's 402 is the final word.
func exaLimit(k confstore.Key, freeUSD float64) (*int64, bool) {
	switch {
	case k.BudgetMonthly > 0:
		b := int64(math.Round(k.BudgetMonthly * 1e6))
		return &b, false
	case k.Plan == "free" && freeUSD > 0:
		b := int64(math.Round(freeUSD * 1e6))
		return &b, true
	}
	return nil, false
}

// scopeOf groups keys that share upstream rate limits: per team when a team is
// configured (Exa), otherwise per key.
func scopeOf(k confstore.Key) string {
	if k.Team != "" {
		return "team:" + k.Provider + ":" + k.Team
	}
	return "key:" + k.ID
}

// bucket returns the token bucket enforcing an upstream limit. Refill is 80%
// of the limit and burst the other 20%, so at most limit.Count requests can
// pass in ANY window of limit.Window, however upstream aligns its windows
// (burst + refill*window = 0.2L + 0.8L = L).
func (p *Pool) bucket(k confstore.Key, l provider.Limit) *rate.Limiter {
	id := scopeOf(k) + "|" + l.Bucket
	lim := rate.Limit(0.8 * l.Count / l.Window.Seconds())
	burst := max(1, int(0.2*l.Count))
	b := p.buckets[id]
	if b == nil || b.Limit() != lim || b.Burst() != burst {
		b = rate.NewLimiter(lim, burst)
		p.buckets[id] = b
	}
	return b
}

// rateWait is how long until every limit of this call has a token (0 = now).
func (p *Pool) rateWait(st *keyState, r Request, now time.Time) time.Duration {
	wait := func(b *rate.Limiter) time.Duration {
		t := b.TokensAt(now)
		if t >= 1 {
			return 0
		}
		return time.Duration((1 - t) / float64(b.Limit()) * float64(time.Second))
	}
	var d time.Duration
	if st.limiter != nil {
		d = wait(st.limiter)
	}
	if r.Limits != nil {
		for _, l := range r.Limits(st.cfg) {
			d = max(d, wait(p.bucket(st.cfg, l)))
		}
	}
	return d
}

func limiterFor(rpm int, cur *rate.Limiter) *rate.Limiter {
	if rpm <= 0 {
		return nil
	}
	lim := rate.Limit(float64(rpm) / 60)
	if cur != nil && cur.Limit() == lim {
		return cur
	}
	return rate.NewLimiter(lim, max(1, rpm/10)) // small burst so a key is never hammered
}

func restoreState(st *keyState, ps store.KeyState) {
	if ps.Status != "" {
		st.status = Status(ps.Status)
	}
	st.until, st.reason, st.failScore = ps.StatusUntil, ps.StatusReason, ps.FailScore
	st.limit, st.used = ps.QuotaLimit, ps.QuotaUsed
	if ps.QuotaUnit != "" {
		st.unit = ps.QuotaUnit
	}
	if !ps.PeriodResetAt.IsZero() {
		st.periodEnd = ps.PeriodResetAt
	}
	st.lastSynced, st.lastUsed = ps.LastSyncedAt, ps.LastUsedAt
	st.plan, st.syncErr, st.nextSync = ps.QuotaPlan, ps.SyncError, ps.NextSyncAt
}

// Flush persists changed state. Run it every few seconds and at shutdown.
func (p *Pool) Flush(ctx context.Context) error {
	if p.opts.Store == nil {
		return nil
	}
	p.mu.Lock()
	var batch []store.KeyState
	now := p.opts.Now()
	for _, id := range p.order {
		st := p.keys[id]
		if !st.dirty {
			continue
		}
		st.dirty = false
		batch = append(batch, toStore(st, now))
	}
	removed, restored := p.removed, p.restored
	p.removed, p.restored = nil, nil
	p.mu.Unlock()

	var errs []error
	if err := p.opts.Store.UpsertKeyStates(ctx, batch); err != nil {
		errs = append(errs, err)
		p.markDirty(batch)
	}
	if err := p.opts.Store.RestoreKeys(ctx, restored); err != nil {
		errs = append(errs, err)
	}
	if err := p.opts.Store.MarkKeysRemoved(ctx, removed, now); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("pool flush: %v", errs)
	}
	return nil
}

func (p *Pool) markDirty(batch []store.KeyState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range batch {
		if st := p.keys[s.KeyID]; st != nil {
			st.dirty = true
		}
	}
}

func toStore(st *keyState, now time.Time) store.KeyState {
	var remaining *int64
	if st.limit != nil && st.used != nil {
		r := max(*st.limit-*st.used, 0)
		remaining = &r
	}
	return store.KeyState{KeyID: st.cfg.ID, Provider: st.cfg.Provider, Status: string(st.status), StatusUntil: st.until,
		StatusReason: st.reason, FailScore: st.failScore, QuotaLimit: st.limit, QuotaUsed: st.used, QuotaRemaining: remaining,
		QuotaUnit: st.unit, PeriodResetAt: st.periodEnd, LastSyncedAt: st.lastSynced, LastUsedAt: st.lastUsed, UpdatedAt: now,
		QuotaPlan: st.plan, SyncError: st.syncErr, NextSyncAt: st.nextSync}
}

// ---- picking ------------------------------------------------------------

// Request asks for a key for one attempt.
type Request struct {
	Provider string
	Subject  string          // affinity subject: token ID or MCP session (§6.2 step 3)
	KeyGroup string          // all | free | paid
	Exclude  map[string]bool // keys already tried in this request
	PinKey   string          // async task follow-up: only this key may be used
	// Limits lists the upstream rate limits this call consumes on a key (nil = none).
	Limits func(confstore.Key) []provider.Limit
}

// Lease is a picked key. Exactly one of Done or Abort must be called.
type Lease struct {
	Key  confstore.Key
	p    *Pool
	done bool
}

// NoKeyError explains why no key could be used: reason → number of keys.
type NoKeyError struct {
	Provider string
	Counts   map[string]int
	RetryIn  time.Duration // > 0: a key becomes usable after this wait (only rate limits were in the way)
}

func (e *NoKeyError) Error() string {
	if len(e.Counts) == 0 {
		return fmt.Sprintf("no %s keys configured", e.Provider)
	}
	reasons := make([]string, 0, len(e.Counts))
	for r := range e.Counts {
		if r != "already_tried" { // only meaningful inside one request
			reasons = append(reasons, r)
		}
	}
	if len(reasons) == 0 {
		return fmt.Sprintf("no available %s key", e.Provider)
	}
	sort.Strings(reasons)
	parts := make([]string, len(reasons))
	for i, r := range reasons {
		parts[i] = fmt.Sprintf("%d %s", e.Counts[r], r)
	}
	return fmt.Sprintf("no available %s key (%s)", e.Provider, strings.Join(parts, ", "))
}

// refresh moves expired cooldowns back to active and rolls quota periods.
func (st *keyState) refresh(now time.Time) {
	if !st.periodEnd.IsZero() && !now.Before(st.periodEnd) {
		st.periodEnd = nextReset(now, st.cfg.ResetDay)
		zero := int64(0)
		st.used, st.needSync = &zero, true // confirm the new balance upstream (§10.2)
		if st.status == StatusExhausted {
			st.status, st.until, st.reason = StatusActive, time.Time{}, ""
		}
		st.dirty = true
	}
	if (st.status == StatusCooldown || st.status == StatusExhausted) && !st.until.IsZero() && !now.Before(st.until) {
		st.status, st.until, st.reason = StatusActive, time.Time{}, ""
		st.dirty = true
	}
}

// unavailable returns why st cannot take a request now ("" = usable).
func (p *Pool) unavailable(st *keyState, r Request, now time.Time) string {
	st.refresh(now)
	k := st.cfg
	switch {
	case !k.Enabled:
		return string(StatusDisabled)
	case r.KeyGroup == "free" && k.Plan != "free", r.KeyGroup == "paid" && k.Plan != "paid":
		return "not_in_group"
	case r.Exclude[k.ID]:
		return "already_tried"
	case st.status != StatusActive:
		return string(st.status)
	case p.overBudget(st):
		return "budget_reached"
	case !k.Egress.Enabled || (p.opts.EgressUsable != nil && !p.opts.EgressUsable(k.Egress.ID)):
		return string(StatusEgressDown) // hard binding: never fall back to direct (§7.2)
	case p.routing.PerKeyConcurrency > 0 && st.inFlight >= p.routing.PerKeyConcurrency:
		return "busy"
	case p.rateWait(st, r, now) > 0:
		return "rate_limited"
	}
	return ""
}

// overBudget: the upstream balance is used up, or a paid key hit its own monthly cap.
func (p *Pool) overBudget(st *keyState) bool {
	if st.used == nil {
		return false
	}
	if st.limit != nil && !st.limitSoft && *st.used >= *st.limit {
		return true
	}
	if st.cfg.Provider == "tavily" && st.cfg.BudgetMonthly > 0 && float64(*st.used) >= st.cfg.BudgetMonthly {
		return true
	}
	return false
}

// Pick chooses a key for one attempt.
func (p *Pool) Pick(r Request) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	counts := map[string]int{}

	if r.PinKey != "" {
		st := p.keys[r.PinKey]
		if st == nil || st.cfg.Provider != r.Provider {
			return nil, &NoKeyError{Provider: r.Provider, Counts: map[string]int{"task_key_removed": 1}}
		}
		pin := r
		pin.KeyGroup, pin.Exclude = "all", nil
		if why := p.unavailable(st, pin, now); why != "" && why != "rate_limited" && why != "busy" {
			return nil, &NoKeyError{Provider: r.Provider, Counts: map[string]int{"task_key_" + why: 1}}
		}
		return p.take(st, pin, now), nil
	}

	var cands []*keyState
	var retryIn time.Duration
	bestPrio := math.MaxInt
	for _, id := range p.order {
		st := p.keys[id]
		if st.cfg.Provider != r.Provider {
			continue
		}
		if why := p.unavailable(st, r, now); why != "" {
			counts[why]++
			if why == "rate_limited" {
				if w := p.rateWait(st, r, now); retryIn == 0 || w < retryIn {
					retryIn = w
				}
			}
			continue
		}
		if st.cfg.Priority < bestPrio { // tiers: free keys before paid ones by default
			bestPrio, cands = st.cfg.Priority, cands[:0]
		}
		if st.cfg.Priority == bestPrio {
			cands = append(cands, st)
		}
	}
	if len(cands) == 0 {
		return nil, &NoKeyError{Provider: r.Provider, Counts: counts, RetryIn: retryIn}
	}
	return p.take(p.choose(cands, r, now), r, now), nil
}

func (p *Pool) take(st *keyState, r Request, now time.Time) *Lease {
	st.inFlight++
	st.lastUsed = now
	st.dirty = true
	if st.limiter != nil {
		st.limiter.AllowN(now, 1)
	}
	if r.Limits != nil {
		for _, l := range r.Limits(st.cfg) {
			p.bucket(st.cfg, l).AllowN(now, 1)
		}
	}
	return &Lease{Key: st.cfg, p: p}
}

func (p *Pool) choose(cands []*keyState, r Request, now time.Time) *keyState {
	weighted := unequalWeights(cands)
	switch p.routing.Strategy {
	case "drain-first":
		return cands[0] // file order: finish one key before touching the next
	case "round-robin":
		if weighted {
			return smoothWeighted(cands)
		}
		i := p.rr[r.Provider] % uint64(len(cands))
		p.rr[r.Provider]++
		return cands[i]
	case "random":
		if weighted {
			return p.weightedRandom(cands)
		}
		return cands[p.rnd.IntN(len(cands))]
	}
	if weighted {
		// Explicit weights are a share the admin asked for: honour it exactly
		// instead of balancing remaining quota (affinity would skew it too).
		return smoothWeighted(cands)
	}
	// balanced: a subject sticks to a few keys for a while (rendezvous hashing,
	// rotated every affinity window), then the best of those wins.
	if r.Subject != "" && p.routing.AffinityMinutes > 0 && len(cands) > affinityTopN {
		epoch := now.Unix() / int64(p.routing.AffinityMinutes*60)
		sort.SliceStable(cands, func(i, j int) bool {
			return rendezvous(r.Subject, epoch, cands[i].cfg.ID) > rendezvous(r.Subject, epoch, cands[j].cfg.ID)
		})
		cands = cands[:affinityTopN]
	}
	best, bestScore := cands[0], math.Inf(-1)
	for _, st := range cands {
		if s := p.score(st, now); s > bestScore {
			best, bestScore = st, s
		}
	}
	return best
}

// weightOf is a key's weight; unset (0) counts as 1.
func weightOf(k confstore.Key) float64 {
	if k.Weight <= 0 {
		return 1
	}
	return k.Weight
}

func unequalWeights(cands []*keyState) bool {
	for _, st := range cands[1:] {
		if weightOf(st.cfg) != weightOf(cands[0].cfg) {
			return true
		}
	}
	return false
}

// smoothWeighted is nginx's smooth weighted round-robin: over any run of picks
// each key gets its share of weight, interleaved rather than in bursts. Keys
// that are unavailable this round are simply not candidates.
func smoothWeighted(cands []*keyState) *keyState {
	var total float64
	best := cands[0]
	for _, st := range cands {
		w := weightOf(st.cfg)
		st.cw += w
		total += w
		if st.cw > best.cw {
			best = st
		}
	}
	best.cw -= total
	return best
}

func (p *Pool) weightedRandom(cands []*keyState) *keyState {
	var total float64
	for _, st := range cands {
		total += weightOf(st.cfg)
	}
	x := p.rnd.Float64() * total
	for _, st := range cands {
		if x -= weightOf(st.cfg); x < 0 {
			return st
		}
	}
	return cands[len(cands)-1]
}

func rendezvous(subject string, epoch int64, keyID string) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%d|%s", subject, epoch, keyID)
	return h.Sum64()
}

// score prefers keys with more balance left, idle keys and healthy keys.
func (p *Pool) score(st *keyState, now time.Time) float64 {
	frac := 0.5
	if st.limit != nil && st.used != nil && *st.limit > 0 {
		frac = math.Max(0, float64(*st.limit-*st.used)/float64(*st.limit))
	}
	idle := 1.0
	if !st.lastUsed.IsZero() {
		idle = math.Min(now.Sub(st.lastUsed).Seconds()/60, 1)
	}
	return 2*frac + idle - st.failScore - 0.5*float64(st.inFlight) + 0.3*p.rnd.Float64()
}

// ---- outcomes -----------------------------------------------------------

// Done records the result of the attempt and releases the lease.
func (l *Lease) Done(o provider.Outcome, cost provider.Cost) {
	if l.done {
		return
	}
	l.done = true
	p := l.p
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.keys[l.Key.ID]
	if st == nil { // removed from config meanwhile
		return
	}
	now := p.opts.Now()
	st.inFlight = max(st.inFlight-1, 0)
	st.dirty = true
	switch o.Class {
	case provider.ClassOK:
		st.failScore *= 0.5
		st.hits429 = 0
		p.charge(st, cost)
	case provider.ClassClientError:
		p.charge(st, cost) // the request was wrong, not the key
	case provider.ClassKeyInvalid:
		reason := reasonOr(o.Reason, "invalid or revoked key (401)")
		markInvalid(st, reason)
		if o.TeamWide { // e.g. Exa TEAM_BLOCKED: the whole team lost access
			for _, sib := range p.teamOf(st) {
				markInvalid(sib, reason+" (team)")
			}
		}
	case provider.ClassKeyExhausted:
		reason := reasonOr(o.Reason, "quota exhausted")
		markExhausted(st, reason)
		if o.TeamWide { // e.g. Exa NO_MORE_CREDITS: the team's balance is gone for every key
			for _, sib := range p.teamOf(st) {
				if sib.status == StatusActive || sib.status == StatusCooldown {
					markExhausted(sib, reason+" (shared team balance)")
				}
			}
		}
	case provider.ClassRateLimited:
		st.hits429++
		until := now.Add(p.cooldownFor(o, st.hits429))
		st.status, st.until, st.reason = StatusCooldown, until, "rate limited (429)"
		if o.TeamWide { // a sibling key would hit the same team limit
			for _, sib := range p.teamOf(st) {
				if sib.status == StatusActive {
					sib.status, sib.until, sib.reason = StatusCooldown, until, "rate limited (429, shared team limit)"
					sib.dirty = true
				}
			}
		}
	case provider.ClassKeyForbidden:
		st.status, st.until, st.reason = StatusCooldown, now.Add(cooldownForbidden), reasonOr(o.Reason, "forbidden (403)")
	case provider.ClassEdgeBlocked:
		// The egress IP is blocked, not the key: the egress layer quarantines the egress.
	case provider.ClassServerError:
		st.failScore++
		if st.failScore >= serverErrorTrip {
			st.status, st.until, st.reason = StatusCooldown, now.Add(cooldownServerErr), "repeated upstream errors"
			st.failScore = 0
		}
	}
}

// Abort releases a lease whose request never reached upstream (e.g. the egress
// could not connect): no penalty for the key.
func (l *Lease) Abort() {
	if l.done {
		return
	}
	l.done = true
	l.p.mu.Lock()
	defer l.p.mu.Unlock()
	if st := l.p.keys[l.Key.ID]; st != nil {
		st.inFlight = max(st.inFlight-1, 0)
	}
}

// teamOf lists the other keys of st's team (Exa); none when st has no team.
func (p *Pool) teamOf(st *keyState) []*keyState {
	if st.cfg.Team == "" {
		return nil
	}
	var out []*keyState
	for _, other := range p.keys {
		if other != st && other.cfg.Provider == st.cfg.Provider && other.cfg.Team == st.cfg.Team {
			out = append(out, other)
		}
	}
	return out
}

func markInvalid(st *keyState, reason string) {
	st.status, st.until, st.reason, st.dirty = StatusInvalid, time.Time{}, reason, true
}

// markExhausted parks a key until its period ends. A known, enforced limit is
// marked as used up, so the balance shown agrees with upstream.
func markExhausted(st *keyState, reason string) {
	st.status, st.until, st.reason, st.dirty = StatusExhausted, st.periodEnd, reason, true
	if st.limit != nil && !st.limitSoft {
		u := *st.limit
		st.used = &u
	}
}

func reasonOr(r, def string) string {
	if r != "" {
		return r
	}
	return def
}

// cooldownFor honors Retry-After; without it the provider's base doubles per hit.
func (p *Pool) cooldownFor(o provider.Outcome, hits int) time.Duration {
	base, ceiling := o.CooldownBase, o.CooldownMax
	if base <= 0 {
		base = cooldownDefault
	}
	if ceiling <= 0 {
		ceiling = cooldownMax
	}
	d := o.RetryAfter
	if d <= 0 {
		d = base << min(hits-1, 6)
	}
	return p.jitter(min(d, ceiling))
}

// jitter only ever lengthens: a Retry-After must not be undercut.
func (p *Pool) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (1 + 0.2*p.rnd.Float64()))
}

func (p *Pool) charge(st *keyState, c provider.Cost) {
	units := c.Credits
	if st.cfg.Provider == "exa" {
		units = c.MicroUSD
	}
	if units <= 0 {
		return
	}
	u := units
	if st.used != nil {
		u += *st.used
	}
	st.used = &u
}

// SetQuota merges a balance synced from upstream (Tavily GET /usage).
//
// Tavily's usage counters lag behind real usage (observed 2026-11-10: updated
// in batches, 6 to 45 minutes after the billed calls), so a sync must not erase what the pool
// has already charged locally. Both numbers are lower bounds of the truth, so
// the larger one wins — except when upstream reports balance left while the
// local count says exhausted: then upstream has reset (its billing cycle may
// differ from reset_day) and is trusted.
func (p *Pool) SetQuota(id string, q provider.Quota, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.keys[id]
	if st == nil {
		return
	}
	st.limit, st.limitSoft = q.Limit, false // nil: upstream reports no limit
	if q.Used != nil {
		used := *q.Used
		if st.used != nil && *st.used > used {
			localExhausted := st.limit != nil && *st.used >= *st.limit
			upstreamHasBalance := q.Remaining != nil && *q.Remaining > 0
			if !(localExhausted && upstreamHasBalance) {
				used = *st.used // upstream is lagging behind our own charges
			}
		}
		st.used = &used
	}
	st.plan, st.syncErr, st.needSync = q.Plan, "", false
	st.lastSynced, st.dirty = at, true
	if st.status == StatusInvalid {
		// The key just authenticated, so whatever failed before is gone (§6.1 retest).
		st.status, st.until, st.reason = StatusActive, time.Time{}, ""
	}
	left := remaining(st)
	switch {
	case left == nil:
	case *left > 0 && st.status == StatusExhausted:
		st.status, st.until, st.reason = StatusActive, time.Time{}, "" // e.g. plan upgraded or cycle reset upstream
	case *left == 0 && st.status == StatusActive:
		markExhausted(st, "no credits left (quota sync)")
	}
}

// SyncFailed records a failed quota sync. A 401 means the key itself is bad,
// exactly as on a normal call; any other failure is only reported.
func (p *Pool) SyncFailed(id string, o provider.Outcome, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.keys[id]
	if st == nil {
		return
	}
	st.syncErr, st.dirty = msg, true
	if o.Class == provider.ClassKeyInvalid {
		markInvalid(st, reasonOr(o.Reason, "invalid or revoked key (401)"))
	}
}

// ProbeOK records that upstream accepted the key (a key test without a
// balance API): an invalid key is active again and a past error is cleared.
func (p *Pool) ProbeOK(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.keys[id]
	if st == nil {
		return
	}
	st.syncErr, st.dirty = "", true
	if st.status == StatusInvalid {
		st.status, st.until, st.reason = StatusActive, time.Time{}, ""
	}
}

// DeferSync sets the earliest time of the key's next quota sync.
func (p *Pool) DeferSync(id string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.keys[id]; st != nil {
		st.nextSync, st.dirty = until, true
	}
}

// remaining is limit - used, nil when either is unknown.
func remaining(st *keyState) *int64 {
	if st.limit == nil || st.used == nil {
		return nil
	}
	r := max(*st.limit-*st.used, 0)
	return &r
}

// ---- views --------------------------------------------------------------

// View is a key's state for the panel and the API.
type View struct {
	ID, Provider, Label, Plan, Egress string
	Status                            Status
	Until                             time.Time
	Reason                            string
	Limit, Used, Remaining            *int64
	Unit                              string
	PeriodResetAt, LastSyncedAt       time.Time
	LastUsedAt                        time.Time
	InFlight                          int
	FailScore                         float64
	Team                              string
	UpstreamPlan                      string // plan name from the last quota sync
	LimitEstimated                    bool   // Limit is an estimate (Exa free grant), not enforced
	SharedRemaining                   bool   // Remaining is the team's, shared with its other keys
	SyncError                         string // last quota sync error
	NextSyncAt                        time.Time
	NeedSync                          bool
}

// Views lists every key in config order.
func (p *Pool) Views() []View {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.opts.Now()
	// An estimated (Exa free) balance belongs to the team: every key of it spends from it.
	teamUsed := map[string]int64{}
	for _, st := range p.keys {
		if st.cfg.Team != "" && st.used != nil {
			teamUsed[scopeOf(st.cfg)] += *st.used
		}
	}
	out := make([]View, 0, len(p.order))
	for _, id := range p.order {
		st := p.keys[id]
		st.refresh(now)
		status := st.status
		switch {
		case !st.cfg.Enabled:
			status = StatusDisabled
		case status == StatusActive && (!st.cfg.Egress.Enabled || (p.opts.EgressUsable != nil && !p.opts.EgressUsable(st.cfg.Egress.ID))):
			status = StatusEgressDown
		}
		v := View{ID: id, Provider: st.cfg.Provider, Label: st.cfg.DisplayName(), Plan: st.cfg.Plan, Egress: st.cfg.Egress.Label(),
			Status: status, Until: st.until, Reason: st.reason, Limit: st.limit, Used: st.used, Unit: st.unit,
			PeriodResetAt: st.periodEnd, LastSyncedAt: st.lastSynced, LastUsedAt: st.lastUsed, InFlight: st.inFlight, FailScore: st.failScore,
			Team: st.cfg.Team, UpstreamPlan: st.plan, LimitEstimated: st.limitSoft, SyncError: st.syncErr, NextSyncAt: st.nextSync, NeedSync: st.needSync}
		switch {
		case st.limit != nil && st.limitSoft:
			used := int64(0) // nothing charged yet this period
			if st.used != nil {
				used = *st.used
			}
			if st.cfg.Team != "" {
				used, v.SharedRemaining = teamUsed[scopeOf(st.cfg)], true
			}
			r := max(*st.limit-used, 0)
			v.Remaining = &r
		default:
			v.Remaining = remaining(st)
		}
		out = append(out, v)
	}
	return out
}
