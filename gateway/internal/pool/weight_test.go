package pool

import (
	"testing"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
)

func weighted(secret string, w float64, prio int) confstore.Key {
	k := key("tavily", secret, "free", prio)
	k.Weight = w
	return k
}

func countPicks(f *fixture, n int, r Request) map[string]int {
	got := map[string]int{}
	for i := 0; i < n; i++ {
		l := f.pick(r)
		got[l.Key.Secret]++
		l.Done(ok(), provider.Cost{})
	}
	return got
}

func TestWeightsSplitTrafficExactly(t *testing.T) {
	for _, strategy := range []string{"balanced", "round-robin"} {
		f := newFixture(t, confstore.RoutingConfig{Strategy: strategy, AffinityMinutes: 30},
			weighted("a", 5, 0), weighted("b", 3, 0), weighted("c", 2, 0))
		got := countPicks(f, 1000, Request{Provider: "tavily", Subject: "token-1"})
		if got["a"] != 500 || got["b"] != 300 || got["c"] != 200 {
			t.Fatalf("%s: picks = %v", strategy, got)
		}
		// Smooth: within any 10 consecutive picks the split is exact, no bursts.
		seq := ""
		for i := 0; i < 10; i++ {
			l := f.pick(Request{Provider: "tavily"})
			seq += l.Key.Secret
			l.Done(ok(), provider.Cost{})
		}
		if seq != "abcabacbaa" && countRune(seq, 'a') != 5 {
			t.Fatalf("%s: sequence %s", strategy, seq)
		}
	}
}

func countRune(s string, r rune) int {
	n := 0
	for _, c := range s {
		if c == r {
			n++
		}
	}
	return n
}

func TestWeightsSkipUnavailableAndKeepTiers(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, weighted("a", 3, 0), weighted("b", 1, 0), weighted("c", 1, 0), weighted("paid", 100, 1))
	// a is cooling down: b and c (equal weights: balanced) share its traffic, the paid tier stays unused.
	l, _ := f.p.Pick(Request{Provider: "tavily", Exclude: map[string]bool{f.snap.Keys[1].ID: true, f.snap.Keys[2].ID: true}})
	l.Done(provider.Outcome{Class: provider.ClassRateLimited}, provider.Cost{})
	got := countPicks(f, 100, Request{Provider: "tavily"})
	if got["a"] != 0 || got["b"]+got["c"] != 100 || got["b"] < 35 || got["c"] < 35 || got["paid"] != 0 {
		t.Fatalf("picks = %v", got)
	}
}

func TestEqualWeightsKeepBalancedBehaviour(t *testing.T) {
	// Default weights (unset or all equal): balanced still prefers the key with more quota left.
	f := newFixture(t, confstore.RoutingConfig{}, weighted("a", 0, 0), weighted("b", 0, 0))
	f.p.SetQuota(f.snap.Keys[0].ID, provider.Quota{Used: ptr(900), Limit: ptr(1000), Remaining: ptr(100)}, f.clock)
	f.p.SetQuota(f.snap.Keys[1].ID, provider.Quota{Used: ptr(100), Limit: ptr(1000), Remaining: ptr(900)}, f.clock)
	if l := f.pick(Request{Provider: "tavily"}); l.Key.Secret != "b" {
		t.Fatalf("picked %s", l.Key.Secret)
	}
}

func TestWeightedRandom(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{Strategy: "random"}, weighted("a", 9, 0), weighted("b", 1, 0))
	got := countPicks(f, 2000, Request{Provider: "tavily"})
	if got["a"] < 1700 || got["a"] > 1900 {
		t.Fatalf("picks = %v", got)
	}
}
