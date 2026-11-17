package pool

import (
	"errors"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
)

func exaKey(secret, team string) confstore.Key {
	k := key("exa", secret, "free", 0)
	k.Team = team
	return k
}

func search10QPS(confstore.Key) []provider.Limit {
	return []provider.Limit{{Bucket: "search", Count: 10, Window: time.Second}}
}

func TestBucketNeverExceedsDocumentedLimit(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, exaKey("exa-a", ""))
	var accepted []time.Time
	end := f.clock.Add(10 * time.Second)
	for f.clock.Before(end) { // hammer it every 5 ms
		if l, err := f.p.Pick(Request{Provider: "exa", Limits: search10QPS}); err == nil {
			accepted = append(accepted, f.clock)
			l.Done(ok(), provider.Cost{})
		}
		f.clock = f.clock.Add(5 * time.Millisecond)
	}
	for i, start := range accepted {
		n := 0
		for _, ts := range accepted[i:] {
			if ts.Sub(start) < time.Second {
				n++
			}
		}
		if n > 10 {
			t.Fatalf("%d requests inside one second starting at %v", n, start)
		}
	}
	if len(accepted) < 75 || len(accepted) > 85 { // burst 2 + 8/s for 10 s
		t.Fatalf("accepted %d requests in 10 s, want about 82", len(accepted))
	}
}

func TestTeamSharesLimitAndRetryHint(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{Strategy: "round-robin"}, exaKey("exa-a", "t1"), exaKey("exa-b", "t1"), exaKey("exa-c", "t2"))
	other := f.snap.Keys[2].ID
	r := Request{Provider: "exa", Limits: search10QPS, Exclude: map[string]bool{other: true}}
	f.pick(r).Done(ok(), provider.Cost{})
	f.pick(r).Done(ok(), provider.Cost{}) // team t1's burst of 2 is now spent
	_, err := f.p.Pick(r)
	var nk *NoKeyError
	if !errors.As(err, &nk) || nk.Counts["rate_limited"] != 2 || nk.RetryIn <= 0 || nk.RetryIn > 150*time.Millisecond {
		t.Fatalf("err = %v (%+v)", err, nk)
	}
	if l := f.pick(Request{Provider: "exa", Limits: search10QPS}); l.Key.ID != other {
		t.Fatalf("the other team has its own limit, got %s", l.Key.Label)
	}
	f.clock = f.clock.Add(nk.RetryIn + time.Millisecond)
	f.pick(r).Done(ok(), provider.Cost{})
}

func TestKeysWithoutTeamAreSeparateAndBucketsIndependent(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{Strategy: "drain-first"}, exaKey("exa-a", ""), exaKey("exa-b", ""))
	first, second := f.snap.Keys[0].ID, f.snap.Keys[1].ID
	r := Request{Provider: "exa", Limits: search10QPS}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		l := f.pick(r)
		seen[l.Key.ID]++
		l.Done(ok(), provider.Cost{})
	}
	if seen[first] != 2 || seen[second] != 2 {
		t.Fatalf("each key has its own burst: %v", seen)
	}
	// Search capacity is gone, but /contents is a separate, larger bucket.
	contents := func(confstore.Key) []provider.Limit {
		return []provider.Limit{{Bucket: "contents", Count: 100, Window: time.Second}}
	}
	f.pick(Request{Provider: "exa", Limits: contents}).Done(ok(), provider.Cost{})
}

func TestTeamWide429AndShortRetryAfter(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{Strategy: "drain-first"}, exaKey("exa-a", "t1"), exaKey("exa-b", "t1"), exaKey("exa-c", "t2"))
	f.pick(Request{Provider: "exa"}).Done(provider.Outcome{Class: provider.ClassRateLimited, Status: 429, RetryAfter: 2 * time.Second, TeamWide: true}, provider.Cost{})
	v := f.p.Views()
	if v[0].Status != StatusCooldown || v[1].Status != StatusCooldown || v[2].Status != StatusActive {
		t.Fatalf("statuses = %v %v %v", v[0].Status, v[1].Status, v[2].Status)
	}
	if d := v[0].Until.Sub(f.clock); d < 2*time.Second || d > 2400*time.Millisecond {
		t.Fatalf("Retry-After must be honored, never undercut: %v", d)
	}
	f.clock = f.clock.Add(3 * time.Second)
	if v := f.p.Views(); v[0].Status != StatusActive || v[1].Status != StatusActive {
		t.Fatal("cooldown did not end")
	}
}

func TestCooldownBackoffWithoutRetryAfter(t *testing.T) {
	f := newFixture(t, confstore.RoutingConfig{}, exaKey("exa-a", ""))
	limited := provider.Outcome{Class: provider.ClassRateLimited, CooldownBase: 2 * time.Second, CooldownMax: time.Minute}
	for hit, base := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
		f.pick(Request{Provider: "exa"}).Done(limited, provider.Cost{})
		v := f.p.Views()[0]
		if d := v.Until.Sub(f.clock); d < base || d > base+base/5 {
			t.Fatalf("hit %d: cooldown %v, want about %v", hit+1, d, base)
		}
		f.clock = v.Until.Add(time.Millisecond)
	}
	f.pick(Request{Provider: "exa"}).Done(ok(), provider.Cost{}) // success resets the backoff
	f.pick(Request{Provider: "exa"}).Done(limited, provider.Cost{})
	if d := f.p.Views()[0].Until.Sub(f.clock); d > 2400*time.Millisecond {
		t.Fatalf("backoff not reset: %v", d)
	}
}
