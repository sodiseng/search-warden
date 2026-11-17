package relay

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/sodiseng/search-warden/gateway/internal/limits"
	"github.com/sodiseng/search-warden/gateway/internal/provider"
)

func TestTokenLimitsOnREST(t *testing.T) {
	lim := limits.New(nil, nil, nil)
	f := newFixture(t, opts{limits: lim, tokens: "tokens:\n  - name: capped\n    token: " + clientToken + "\n    limits:\n      daily_requests: 2\n      monthly_credits: 100\n"})
	f.up.handler = ok(`{"query":"golang","results":[],"usage":{"credits":2}}`)

	for i := 0; i < 2; i++ {
		if r := f.search(); r.status != 200 {
			t.Fatalf("request %d = %d %s", i, r.status, r.body)
		}
	}
	r := f.search()
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" ||
		!strings.Contains(gjson.Get(r.body, "detail.error").String(), "daily limit of 2 requests") {
		t.Fatalf("over the limit = %d %v %s", r.status, r.header, r.body)
	}
	if n := len(f.up.seen()); n != 2 {
		t.Fatalf("upstream saw %d requests; a rejected one must not reach it", n)
	}
	l, _ := f.rec.last(t)
	if l.ErrorKind != "token_limit" || l.Status != 429 || l.KeyID != "" {
		t.Fatalf("log = %+v", l)
	}
	// The actual credits were charged to the token.
	tok := f.conf.Snapshot().Tokens[0]
	if u := lim.Usage(tok.ID); u.DayRequests != 2 || u.MonthCredits != 4 || u.InFlight != 0 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestUsageViewIsLocal(t *testing.T) {
	lim := limits.New(nil, nil, nil)
	f := newFixture(t, opts{limits: lim, tokens: "tokens:\n  - name: capped\n    token: " + clientToken + "\n    limits:\n      monthly_credits: 100\n"})
	f.up.handler = ok(`{"query":"golang","results":[],"usage":{"credits":2}}`)
	f.search()
	r := f.call("GET", "/tavily/usage", "", nil)
	if r.status != 200 || gjson.Get(r.body, "key.usage").Int() != 2 || gjson.Get(r.body, "key.limit").Int() != 100 ||
		gjson.Get(r.body, "account.plan_usage").Int() != 2 || gjson.Get(r.body, "account.current_plan").String() != "search-warden" {
		t.Fatalf("usage = %d %s", r.status, r.body)
	}
	if n := len(f.up.seen()); n != 1 {
		t.Fatalf("GET /usage reached upstream (%d requests)", n)
	}
	// The parser for the real endpoint reads the gateway's answer too.
	q, err := provider.NewTavily("").ParseUsage([]byte(r.body))
	if err != nil || *q.Used != 2 || *q.Remaining != 98 {
		t.Fatalf("parsed = %+v %v", q, err)
	}
}
