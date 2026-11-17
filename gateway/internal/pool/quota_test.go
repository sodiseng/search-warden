package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
)

func viewOf(t *testing.T, p *Pool, id string) View {
	t.Helper()
	for _, v := range p.Views() {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("no view for %s", id)
	return View{}
}

func TestQuotaSyncDrivesKeyStatus(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, key("tavily", "tvly-a", "free", 0))
	id := f.snap.Keys[0].ID

	// The account has nothing left: the key is parked until its period ends.
	f.p.SetQuota(id, provider.Quota{Used: ptr(30), Limit: ptr(30), Remaining: ptr(0), Plan: "Researcher"}, f.clock)
	v := viewOf(t, f.p, id)
	if v.Status != StatusExhausted || !v.Until.Equal(v.PeriodResetAt) || v.UpstreamPlan != "Researcher" {
		t.Fatalf("view = %+v", v)
	}

	// A failed sync with 401 invalidates the key; a later successful one is the retest (§6.1).
	f.p.SyncFailed(id, provider.Outcome{Class: provider.ClassKeyInvalid, Status: 401}, "invalid key (401)")
	if v := viewOf(t, f.p, id); v.Status != StatusInvalid || v.SyncError != "invalid key (401)" {
		t.Fatalf("after 401: %+v", v)
	}
	f.p.SetQuota(id, provider.Quota{Used: ptr(30), Limit: ptr(1030), Remaining: ptr(1000)}, f.clock)
	if v := viewOf(t, f.p, id); v.Status != StatusActive || v.SyncError != "" {
		t.Fatalf("after a good sync: %+v", v)
	}

	// Other failures are only reported.
	f.p.SyncFailed(id, provider.Outcome{Class: provider.ClassRateLimited, Status: 429}, "rate limited (429)")
	if v := viewOf(t, f.p, id); v.Status != StatusActive || v.SyncError != "rate limited (429)" {
		t.Fatalf("after 429: %+v", v)
	}

	// Gate and error survive a restart.
	next := f.clock.Add(5 * time.Minute)
	f.p.DeferSync(id, next)
	if err := f.p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	p2 := f.newPool()
	if err := p2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	p2.Apply(f.snap)
	if v := viewOf(t, p2, id); !v.NextSyncAt.Equal(next) || v.SyncError != "rate limited (429)" || *v.Used != 30 {
		t.Fatalf("restored = %+v", v)
	}

	// A new period asks for a confirming sync.
	f.clock = v.PeriodResetAt.Add(time.Minute)
	if v := viewOf(t, f.p, id); !v.NeedSync || *v.Used != 0 {
		t.Fatalf("after rollover: %+v", v)
	}
	f.p.SetQuota(id, provider.Quota{Used: ptr(0), Limit: ptr(1000), Remaining: ptr(1000)}, f.clock)
	if v := viewOf(t, f.p, id); v.NeedSync {
		t.Fatal("sync did not clear NeedSync")
	}
}

// Tavily's budget_monthly caps the key's own usage, not its account's.
func TestTavilyBudgetUsesKeyUsage(t *testing.T) {
	k := key("tavily", "tvly-paid", "paid", 1)
	k.BudgetMonthly = 100
	f := newFixture(t, confstore.RoutingConfig{}, k)
	f.p.SetQuota(k.ID, provider.Quota{Used: ptr(90), Limit: ptr(5090), Remaining: ptr(5000)}, f.clock)
	f.pick(Request{Provider: "tavily"}).Done(ok(), provider.Cost{Credits: 10})
	_, err := f.p.Pick(Request{Provider: "tavily"})
	var nk *NoKeyError
	if !errors.As(err, &nk) || nk.Counts["budget_reached"] != 1 {
		t.Fatalf("err = %v", err)
	}
}

func TestExaLedgerEstimateAndBudget(t *testing.T) {
	paid := exaKey("exa-paid", "")
	paid.Plan, paid.BudgetMonthly = "paid", 0.01
	f := newFixture(t, confstore.RoutingConfig{}, exaKey("exa-a", "t1"), exaKey("exa-b", "t1"), exaKey("exa-solo", ""), paid)
	f.snap.Config.Quota.ExaFreeMonthlyUSD = 10
	f.p.Apply(f.snap)
	a, b, solo := f.snap.Keys[0].ID, f.snap.Keys[1].ID, f.snap.Keys[2].ID

	// Spend $3 on key a, far past nothing: free keys show the monthly grant as an estimate…
	only := func(id string) map[string]bool {
		ex := map[string]bool{}
		for _, k := range f.snap.Keys {
			if k.ID != id {
				ex[k.ID] = true
			}
		}
		return ex
	}
	f.pick(Request{Provider: "exa", Exclude: only(a)}).Done(ok(), provider.Cost{MicroUSD: 3_000_000})
	va, vb, vs := viewOf(t, f.p, a), viewOf(t, f.p, b), viewOf(t, f.p, solo)
	if !va.LimitEstimated || *va.Limit != 10_000_000 || *va.Remaining != 7_000_000 || !va.SharedRemaining {
		t.Fatalf("a = %+v", va)
	}
	if *vb.Remaining != 7_000_000 || !vb.SharedRemaining { // same team, same balance
		t.Fatalf("b = %+v", vb)
	}
	if *vs.Remaining != 10_000_000 || vs.SharedRemaining {
		t.Fatalf("solo = %+v", vs)
	}
	// …which is never enforced: upstream's 402 decides.
	f.pick(Request{Provider: "exa", Exclude: only(a)}).Done(ok(), provider.Cost{MicroUSD: 9_000_000})
	if l := f.pick(Request{Provider: "exa", Exclude: only(a)}); l.Key.ID != a {
		t.Fatal("estimate blocked the key")
	}

	// A budget is enforced.
	f.pick(Request{Provider: "exa", KeyGroup: "paid"}).Done(ok(), provider.Cost{MicroUSD: 10_000})
	if _, err := f.p.Pick(Request{Provider: "exa", KeyGroup: "paid"}); err == nil {
		t.Fatal("budget not enforced")
	}
	// Removing the budget removes the limit (a paid key has no free grant to estimate).
	f.snap.Keys[3].BudgetMonthly = 0
	f.p.Apply(f.snap)
	if v := viewOf(t, f.p, paid.ID); v.Limit != nil {
		t.Fatalf("paid key without budget = %+v", v)
	}
	f.pick(Request{Provider: "exa", KeyGroup: "paid"}).Done(ok(), provider.Cost{})

	// No configured grant: no estimate.
	f.snap.Config.Quota.ExaFreeMonthlyUSD = 0
	f.p.Apply(f.snap)
	if v := viewOf(t, f.p, solo); v.Limit != nil || v.Remaining != nil {
		t.Fatalf("solo without grant = %+v", v)
	}
}

func TestTeamWideExhaustionAndBlock(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, exaKey("exa-a", "t1"), exaKey("exa-b", "t1"), exaKey("exa-c", "t2"))
	a, b, c := f.snap.Keys[0].ID, f.snap.Keys[1].ID, f.snap.Keys[2].ID
	pickA := Request{Provider: "exa", Exclude: map[string]bool{b: true, c: true}}

	// The key's own budget: only that key stops.
	f.pick(pickA).Done(provider.Outcome{Class: provider.ClassKeyExhausted, Status: 402}, provider.Cost{})
	if viewOf(t, f.p, a).Status != StatusExhausted || viewOf(t, f.p, b).Status != StatusActive {
		t.Fatal("key-level exhaustion spread to the team")
	}

	// The team's credits: the whole team stops, the other team does not.
	f2 := newFixture(t, confstore.RoutingConfig{}, exaKey("exa-a", "t1"), exaKey("exa-b", "t1"), exaKey("exa-c", "t2"))
	f2.pick(pickA).Done(provider.Outcome{Class: provider.ClassKeyExhausted, Status: 402, TeamWide: true}, provider.Cost{})
	if viewOf(t, f2.p, b).Status != StatusExhausted || viewOf(t, f2.p, c).Status != StatusActive {
		t.Fatalf("team exhaustion: b=%s c=%s", viewOf(t, f2.p, b).Status, viewOf(t, f2.p, c).Status)
	}

	// A blocked team: every key of it is invalid.
	f3 := newFixture(t, confstore.RoutingConfig{}, exaKey("exa-a", "t1"), exaKey("exa-b", "t1"), exaKey("exa-c", "t2"))
	f3.pick(pickA).Done(provider.Outcome{Class: provider.ClassKeyInvalid, Status: 403, TeamWide: true}, provider.Cost{})
	if viewOf(t, f3.p, b).Status != StatusInvalid || viewOf(t, f3.p, c).Status != StatusActive {
		t.Fatal("team block not applied to the team only")
	}
}
