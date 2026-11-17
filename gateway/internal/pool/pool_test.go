package pool

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

type fixture struct {
	t     *testing.T
	p     *Pool
	clock time.Time
	snap  *confstore.Snapshot
	st    *store.Store
	down  map[string]bool
}

func key(provider, secret, plan string, prio int) confstore.Key {
	return confstore.Key{ID: confstore.KeyID(provider, secret), Provider: provider, Secret: secret, Label: secret,
		Plan: plan, Priority: prio, ResetDay: 1, Enabled: true, Egress: egress.Resolved{ID: egress.DirectID, Enabled: true}}
}

func newFixture(t *testing.T, routing confstore.RoutingConfig, keys ...confstore.Key) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(t.TempDir(), "db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if routing.Strategy == "" {
		routing.Strategy = "balanced"
	}
	f := &fixture{t: t, clock: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), st: st, down: map[string]bool{}}
	f.snap = &confstore.Snapshot{Config: confstore.Config{Routing: routing}, Keys: keys}
	f.p = f.newPool()
	f.p.Apply(f.snap)
	return f
}

func (f *fixture) newPool() *Pool {
	return New(Options{Store: f.st, Now: func() time.Time { return f.clock }, Rand: rand.New(rand.NewPCG(1, 2)),
		EgressUsable: func(id string) bool { return !f.down[id] }})
}

func (f *fixture) pick(r Request) *Lease {
	f.t.Helper()
	l, err := f.p.Pick(r)
	if err != nil {
		f.t.Fatalf("pick: %v", err)
	}
	return l
}

func ok() provider.Outcome { return provider.Outcome{Class: provider.ClassOK, Status: 200} }

func TestProviderAndTiering(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{},
		key("tavily", "tvly-free-1", "free", 0), key("tavily", "tvly-paid-1", "paid", 1), key("exa", "exa-1", "free", 0))
	for i := 0; i < 20; i++ {
		l := f.pick(Request{Provider: "tavily"})
		if l.Key.Secret != "tvly-free-1" {
			t.Fatalf("paid key used while a free key is available: %s", l.Key.Secret)
		}
		l.Done(ok(), provider.Cost{})
	}
	if l := f.pick(Request{Provider: "exa"}); l.Key.Provider != "exa" {
		t.Fatal("wrong provider")
	}
	// Free key exhausted → the paid tier takes over.
	l := f.pick(Request{Provider: "tavily"})
	l.Done(provider.Outcome{Class: provider.ClassKeyExhausted, Status: 432}, provider.Cost{})
	if l := f.pick(Request{Provider: "tavily"}); l.Key.Secret != "tvly-paid-1" {
		t.Fatalf("expected paid fallback, got %s", l.Key.Secret)
	}
	// Token restricted to free keys cannot fall through to paid ones.
	_, err := f.p.Pick(Request{Provider: "tavily", KeyGroup: "free"})
	var nk *NoKeyError
	if !errors.As(err, &nk) || nk.Counts["exhausted"] != 1 || nk.Counts["not_in_group"] != 1 {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusTransitions(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, key("tavily", "tvly-a", "free", 0))
	id := f.snap.Keys[0].ID
	status := func() Status { return f.p.Views()[0].Status }

	// 429 with Retry-After far above the cap → clamped to 5 minutes (±20%).
	f.pick(Request{Provider: "tavily"}).Done(provider.Outcome{Class: provider.ClassRateLimited, RetryAfter: time.Hour}, provider.Cost{})
	v := f.p.Views()[0]
	if v.Status != StatusCooldown || v.Until.Sub(f.clock) > 6*time.Minute || v.Until.Sub(f.clock) < 4*time.Minute {
		t.Fatalf("cooldown = %+v", v)
	}
	if _, err := f.p.Pick(Request{Provider: "tavily"}); err == nil {
		t.Fatal("cooling key picked")
	}
	f.clock = f.clock.Add(7 * time.Minute)
	if status() != StatusActive {
		t.Fatal("cooldown did not expire")
	}

	// Three server errors trip a cooldown.
	for i := 0; i < 3; i++ {
		f.pick(Request{Provider: "tavily"}).Done(provider.Outcome{Class: provider.ClassServerError, Status: 502}, provider.Cost{})
	}
	if status() != StatusCooldown {
		t.Fatalf("server errors: %v", status())
	}
	f.clock = f.clock.Add(2 * time.Minute)

	// Exhausted until the monthly reset day, then active again with usage reset.
	f.pick(Request{Provider: "tavily"}).Done(provider.Outcome{Class: provider.ClassKeyExhausted, Status: 432}, provider.Cost{})
	if v := f.p.Views()[0]; v.Status != StatusExhausted || !v.Until.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("exhausted = %+v", v)
	}
	f.clock = time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC)
	if v := f.p.Views()[0]; v.Status != StatusActive || v.Used == nil || *v.Used != 0 {
		t.Fatalf("after reset = %+v", v)
	}

	// 401 → invalid, stays invalid.
	f.pick(Request{Provider: "tavily"}).Done(provider.Outcome{Class: provider.ClassKeyInvalid, Status: 401}, provider.Cost{})
	f.clock = f.clock.Add(48 * time.Hour)
	if status() != StatusInvalid {
		t.Fatal("invalid key recovered by itself")
	}
	_ = id
}

func TestEdgeBlockAndAbortDoNotPenalizeKey(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, key("tavily", "tvly-a", "free", 0))
	f.pick(Request{Provider: "tavily"}).Done(provider.Outcome{Class: provider.ClassEdgeBlocked, Status: 403}, provider.Cost{})
	f.pick(Request{Provider: "tavily"}).Abort()
	if v := f.p.Views()[0]; v.Status != StatusActive || v.InFlight != 0 || v.FailScore != 0 {
		t.Fatalf("view = %+v", v)
	}
}

func TestEgressDownNeverFallsBackToDirect(t *testing.T) {
	k := key("tavily", "tvly-proxied", "free", 0)
	k.Egress = egress.Resolved{ID: "ehk1", Name: "hk-1", URL: "socks5://1.1.1.1", Enabled: true}
	f := newFixture(t, confstore.RoutingConfig{}, k, key("tavily", "tvly-direct", "free", 0))
	f.down["ehk1"] = true
	for i := 0; i < 10; i++ {
		l := f.pick(Request{Provider: "tavily"})
		if l.Key.Secret != "tvly-direct" {
			t.Fatal("key on a down egress was picked")
		}
		l.Done(ok(), provider.Cost{})
	}
	if v := f.p.Views()[0]; v.Status != StatusEgressDown {
		t.Fatalf("view = %+v", v)
	}
}

func TestAffinityAndSpread(t *testing.T) {
	var keys []confstore.Key
	for i := 0; i < 10; i++ {
		keys = append(keys, key("tavily", fmt.Sprintf("tvly-k%02d", i), "free", 0))
	}
	f := newFixture(t, confstore.RoutingConfig{AffinityMinutes: 30}, keys...)
	used := map[string]bool{}
	for i := 0; i < 30; i++ {
		l := f.pick(Request{Provider: "tavily", Subject: "token-A"})
		used[l.Key.ID] = true
		l.Done(ok(), provider.Cost{})
		f.clock = f.clock.Add(10 * time.Second) // still inside the 30-minute window
	}
	if len(used) > affinityTopN {
		t.Fatalf("subject spread over %d keys, want ≤ %d", len(used), affinityTopN)
	}
	all := map[string]bool{}
	for s := 0; s < 200; s++ {
		l := f.pick(Request{Provider: "tavily", Subject: fmt.Sprintf("token-%d", s)})
		all[l.Key.ID] = true
		l.Done(ok(), provider.Cost{})
	}
	if len(all) != 10 {
		t.Fatalf("many subjects used only %d of 10 keys", len(all))
	}
}

func TestStrategiesExcludeAndPin(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{Strategy: "round-robin"},
		key("exa", "exa-1", "free", 0), key("exa", "exa-2", "free", 0), key("exa", "exa-3", "free", 0))
	seq := ""
	for i := 0; i < 6; i++ {
		l := f.pick(Request{Provider: "exa"})
		seq += l.Key.Secret[len(l.Key.Secret)-1:]
		l.Done(ok(), provider.Cost{})
	}
	if seq != "123123" {
		t.Fatalf("round-robin order = %s", seq)
	}
	ex := map[string]bool{f.snap.Keys[0].ID: true, f.snap.Keys[1].ID: true}
	if l := f.pick(Request{Provider: "exa", Exclude: ex}); l.Key.Secret != "exa-3" {
		t.Fatal("exclude ignored")
	}
	pinned := f.snap.Keys[1].ID
	if l := f.pick(Request{Provider: "exa", PinKey: pinned}); l.Key.ID != pinned {
		t.Fatal("pin ignored")
	}
	f.pick(Request{Provider: "exa", PinKey: pinned}).Done(provider.Outcome{Class: provider.ClassKeyInvalid}, provider.Cost{})
	if _, err := f.p.Pick(Request{Provider: "exa", PinKey: pinned}); err == nil {
		t.Fatal("task follow-up must fail rather than move to another key")
	}

	g := newFixture(t, confstore.RoutingConfig{Strategy: "drain-first"}, key("exa", "exa-a", "free", 0), key("exa", "exa-b", "free", 0))
	for i := 0; i < 5; i++ {
		if l := g.pick(Request{Provider: "exa"}); l.Key.Secret != "exa-a" {
			t.Fatal("drain-first must stick to the first key")
		}
	}
}

func TestBudgetAndCharging(t *testing.T) {
	paid := key("tavily", "tvly-paid", "paid", 0)
	paid.BudgetMonthly = 5
	exa := key("exa", "exa-paid", "paid", 0)
	exa.BudgetMonthly = 0.01 // $0.01 = 10000 µUSD
	f := newFixture(t, confstore.RoutingConfig{}, paid, exa)
	for i := 0; i < 2; i++ {
		f.pick(Request{Provider: "tavily"}).Done(ok(), provider.Cost{Credits: 2})
	}
	f.pick(Request{Provider: "tavily"}).Done(ok(), provider.Cost{Credits: 2}) // used = 6 ≥ 5
	if _, err := f.p.Pick(Request{Provider: "tavily"}); err == nil {
		t.Fatal("budget cap ignored")
	}
	f.pick(Request{Provider: "exa"}).Done(ok(), provider.Cost{MicroUSD: 7000})
	f.pick(Request{Provider: "exa"}).Done(ok(), provider.Cost{MicroUSD: 7000})
	var nk *NoKeyError
	if _, err := f.p.Pick(Request{Provider: "exa"}); !errors.As(err, &nk) || nk.Counts["budget_reached"] != 1 {
		t.Fatalf("exa budget: %v", err)
	}
	v := f.p.Views()[1]
	if v.Unit != "micro_usd" || *v.Used != 14000 || *v.Limit != 10000 || *v.Remaining != 0 {
		t.Fatalf("exa view = %+v", v)
	}
}

func TestConcurrencyAndRateLimits(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{PerKeyConcurrency: 1}, key("exa", "exa-1", "free", 0))
	l := f.pick(Request{Provider: "exa"})
	var nk *NoKeyError
	if _, err := f.p.Pick(Request{Provider: "exa"}); !errors.As(err, &nk) || nk.Counts["busy"] != 1 {
		t.Fatalf("concurrency cap: %v", err)
	}
	l.Done(ok(), provider.Cost{})
	f.pick(Request{Provider: "exa"}).Done(ok(), provider.Cost{})

	g := newFixture(t, confstore.RoutingConfig{PerKeyRPM: 10}, key("exa", "exa-1", "free", 0)) // burst 1, one per 6s
	g.pick(Request{Provider: "exa"}).Done(ok(), provider.Cost{})
	if _, err := g.p.Pick(Request{Provider: "exa"}); err == nil {
		t.Fatal("rate limit ignored")
	}
	g.clock = g.clock.Add(7 * time.Second)
	g.pick(Request{Provider: "exa"}).Done(ok(), provider.Cost{})
}

func TestPersistAcrossRestartAndRemoval(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, key("tavily", "tvly-a", "free", 0), key("tavily", "tvly-b", "free", 0))
	a := f.snap.Keys[0]
	f.p.SetQuota(a.ID, provider.Quota{Limit: ptr(1000), Used: ptr(100)}, f.clock)
	l, _ := f.p.Pick(Request{Provider: "tavily", Exclude: map[string]bool{f.snap.Keys[1].ID: true}})
	l.Done(provider.Outcome{Class: provider.ClassRateLimited, RetryAfter: time.Minute}, provider.Cost{})
	if err := f.p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	// "Restart": a new pool loads the state.
	p2 := f.newPool()
	if err := p2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	p2.Apply(f.snap)
	v := p2.Views()[0]
	if v.Status != StatusCooldown || *v.Limit != 1000 || *v.Used != 100 || v.LastSyncedAt.IsZero() {
		t.Fatalf("restored view = %+v", v)
	}

	// Remove key a from config: marked removed in the store; re-adding restores it.
	p2.Apply(&confstore.Snapshot{Config: f.snap.Config, Keys: f.snap.Keys[1:]})
	if err := p2.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	states, _ := f.st.ListKeyStates(context.Background(), "tavily")
	for _, s := range states {
		if s.KeyID == a.ID && s.RemovedAt.IsZero() {
			t.Fatal("removed key not marked")
		}
	}
	p3 := f.newPool()
	_ = p3.Load(context.Background())
	p3.Apply(f.snap)
	_ = p3.Flush(context.Background())
	if v := p3.Views()[0]; *v.Used != 100 {
		t.Fatalf("re-added key lost its state: %+v", v)
	}
	states, _ = f.st.ListKeyStates(context.Background(), "tavily")
	for _, s := range states {
		if s.KeyID == a.ID && !s.RemovedAt.IsZero() {
			t.Fatal("re-added key still marked removed")
		}
	}
}

func TestSetQuotaReactivatesExhaustedKey(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, key("tavily", "tvly-a", "free", 0))
	id := f.snap.Keys[0].ID
	f.pick(Request{Provider: "tavily"}).Done(provider.Outcome{Class: provider.ClassKeyExhausted, Status: 432}, provider.Cost{})
	f.p.SetQuota(id, provider.Quota{Limit: ptr(4000), Used: ptr(10), Remaining: ptr(3990)}, f.clock) // e.g. plan upgraded
	if v := f.p.Views()[0]; v.Status != StatusActive {
		t.Fatalf("view = %+v", v)
	}
}

func TestNoKeysConfigured(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{})
	if _, err := f.p.Pick(Request{Provider: "tavily"}); err == nil || err.Error() != "no tavily keys configured" {
		t.Fatalf("err = %v", err)
	}
}

func TestNextReset(t *testing.T) {
	cases := []struct {
		now  time.Time
		day  int
		want time.Time
	}{
		{time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), 1, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), 15, time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC), 1, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC), 15, time.Date(2026, 12, 15, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := nextReset(c.now, c.day); !got.Equal(c.want) {
			t.Errorf("nextReset(%v, %d) = %v, want %v", c.now, c.day, got, c.want)
		}
	}
}

func ptr(v int64) *int64 { return &v }

func TestSetQuotaMergesWithLocalCharges(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, key("tavily", "tvly-a", "free", 0))
	id := f.snap.Keys[0].ID
	f.p.SetQuota(id, provider.Quota{Limit: ptr(1000), Used: ptr(0), Remaining: ptr(1000)}, f.clock)
	for i := 0; i < 3; i++ {
		f.pick(Request{Provider: "tavily"}).Done(ok(), provider.Cost{Credits: 2})
	}
	// Upstream still reports 0 (lagging): our 6 locally charged credits must survive.
	f.p.SetQuota(id, provider.Quota{Limit: ptr(1000), Used: ptr(0), Remaining: ptr(1000)}, f.clock)
	if v := f.p.Views()[0]; *v.Used != 6 || *v.Remaining != 994 {
		t.Fatalf("lagging sync overwrote local usage: %+v", v)
	}
	// Upstream ahead (key also used outside the gateway): upstream wins.
	f.p.SetQuota(id, provider.Quota{Limit: ptr(1000), Used: ptr(50), Remaining: ptr(950)}, f.clock)
	if v := f.p.Views()[0]; *v.Used != 50 {
		t.Fatalf("upstream-ahead sync ignored: %+v", v)
	}
	// Locally exhausted but upstream has balance (its cycle reset): trust upstream.
	f.p.SetQuota(id, provider.Quota{Limit: ptr(60), Used: ptr(50), Remaining: ptr(10)}, f.clock)
	f.pick(Request{Provider: "tavily"}).Done(ok(), provider.Cost{Credits: 20}) // local 70 ≥ 60
	if _, err := f.p.Pick(Request{Provider: "tavily"}); err == nil {
		t.Fatal("expected local exhaustion")
	}
	f.p.SetQuota(id, provider.Quota{Limit: ptr(60), Used: ptr(3), Remaining: ptr(57)}, f.clock)
	if v := f.p.Views()[0]; *v.Used != 3 || v.Status != StatusActive {
		t.Fatalf("upstream reset not trusted: %+v", v)
	}
}
