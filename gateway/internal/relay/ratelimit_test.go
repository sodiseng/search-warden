package relay

import (
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/sodiseng/search-warden/gateway/internal/pool"
)

const exaOK = `{"results":[],"costDollars":{"total":0.007}}`

func TestShortLocalRateLimitWaitsThenSucceeds(t *testing.T) {
	f := newFixture(t, opts{exaKeys: "  - exa-key-a\n"})
	f.up.handler = ok(exaOK)
	start := time.Now()
	for i := 0; i < 4; i++ { // Exa /search: burst of 2, then 8 per second
		if r := f.call("POST", "/exa/search", `{"query":"x"}`, nil); r.status != 200 {
			t.Fatalf("request %d: %d %s", i, r.status, r.body)
		}
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("requests were not paced: %v", d)
	}
}

func TestRateLimitBeyondWaitBudgetReturns429(t *testing.T) {
	f := newFixture(t, opts{exaKeys: "  - exa-key-a\n", maxWait: time.Millisecond})
	f.up.handler = ok(exaOK)
	for i := 0; i < 2; i++ {
		f.call("POST", "/exa/search", `{"query":"x"}`, nil)
	}
	r := f.call("POST", "/exa/search", `{"query":"x"}`, nil)
	if r.status != 429 || gjson.Get(r.body, "tag").String() != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("status %d retry-after %q: %s", r.status, r.header.Get("Retry-After"), r.body)
	}
	if n := len(f.up.seen()); n != 2 {
		t.Fatalf("upstream calls = %d: the limited call must not be sent", n)
	}
	if l, _ := f.rec.last(t); l.Status != 429 || l.ErrorKind != "rate_limited" {
		t.Fatalf("log = %+v", l)
	}
}

func TestSeparateEndpointLimitsOnTavily(t *testing.T) {
	f := newFixture(t, opts{tavilyKeys: "  - key: tvly-key-a\n", maxWait: time.Millisecond})
	f.up.handler = ok(`{"request_id":"r"}`)
	for i := 0; i < 4; i++ { // research create: 20 RPM → burst of 4
		if r := f.call("POST", "/tavily/research", `{"input":"x"}`, nil); r.status != 200 {
			t.Fatalf("research %d: %d", i, r.status)
		}
	}
	if r := f.call("POST", "/tavily/research", `{"input":"x"}`, nil); r.status != 429 {
		t.Fatalf("fifth research: %d", r.status)
	}
	if r := f.search(); r.status != 200 {
		t.Fatalf("search has its own limit: %d %s", r.status, r.body)
	}
}

func TestFlexTier434IsPassedThroughWithoutPenalty(t *testing.T) {
	f := newFixture(t, opts{})
	f.up.handler = byStatus([]int{434}, "")
	r := f.search()
	if r.status != 434 || len(f.up.seen()) != 1 {
		t.Fatalf("status %d, calls %d", r.status, len(f.up.seen()))
	}
	if v := f.view("tvly-key-a"); v.Status != pool.StatusActive || v.FailScore != 0 {
		t.Fatalf("key penalized: %+v", v)
	}
	if l, _ := f.rec.last(t); l.Outcome != "upstream_busy" {
		t.Fatalf("log = %+v", l)
	}
}
