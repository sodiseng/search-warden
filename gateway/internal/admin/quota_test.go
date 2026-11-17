package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sodiseng/search-warden/gateway/internal/pool"
)

func i64(v int64) *int64 { return &v }

func TestOverviewQuotaAndManualSync(t *testing.T) {
	calls := 0
	views := []pool.View{
		{ID: "t1", Provider: "tavily", Label: "free-1", Status: pool.StatusActive, Used: i64(10), Limit: i64(1000), Remaining: i64(990)},
		{ID: "t2", Provider: "tavily", Label: "free-2", Status: pool.StatusActive, SyncError: "rate limited (429)"},
		// Two keys of one Exa team: one shared, estimated balance.
		{ID: "e1", Provider: "exa", Label: "exa-1", Status: pool.StatusActive, Team: "t", SharedRemaining: true, LimitEstimated: true,
			Used: i64(3_000_000), Limit: i64(10_000_000), Remaining: i64(6_000_000)},
		{ID: "e2", Provider: "exa", Label: "exa-2", Status: pool.StatusActive, Team: "t", SharedRemaining: true, LimitEstimated: true,
			Used: i64(1_000_000), Limit: i64(10_000_000), Remaining: i64(6_000_000)},
	}
	e := newEnv(t, func(o *Options) {
		o.Keys = func() []pool.View { return views }
		o.SyncQuota = func(...string) int { calls++; return 2 }
	})
	c := e.browser()
	e.login(c, pw, "")
	_, body, _ := e.get(c, "/")
	for _, want := range []string{
		"剩余 990 credits（1/2 个 key 已同步）",
		"本月已用 $4.00，剩余 $6.00（免费 key 按每月免费额度估算）",
		"tavily · free-2：rate limited (429)",
		"立即同步 Tavily 额度",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview lacks %q:\n%s", want, body)
		}
	}

	tok := e.csrf(c, "/")
	code, _, h := e.post(c, "/quota/sync", url.Values{"csrf": {tok}})
	if code != http.StatusSeeOther || h.Get("Location") != "/?ok=sync" || calls != 1 {
		t.Fatalf("sync: %d %q calls=%d", code, h.Get("Location"), calls)
	}
	if _, body, _ := e.get(c, "/?ok=sync"); !strings.Contains(body, "已加入同步队列") {
		t.Fatal("no confirmation shown")
	}
	// Without the CSRF token nothing is queued.
	if code, _, _ := e.post(c, "/quota/sync", url.Values{}); code != http.StatusForbidden || calls != 1 {
		t.Fatalf("without CSRF: %d calls=%d", code, calls)
	}
}

func TestQuotaSummaryAndFormatting(t *testing.T) {
	if s := quotaSummary(langZH, "tavily", []pool.View{{Provider: "tavily"}}); s != "尚未同步" {
		t.Errorf("unsynced = %q", s)
	}
	exa := []pool.View{{Provider: "exa", Used: i64(14_000)}} // a paid key without a budget: no balance to show
	if s := quotaSummary(langZH, "exa", exa); s != "本月已用 $0.014" {
		t.Errorf("exa = %q", s)
	}
	if s := quotaSummary(langEN, "exa", exa); s != "Used this month: $0.014" {
		t.Errorf("exa (en) = %q", s)
	}
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := thousands(in); got != want {
			t.Errorf("thousands(%d) = %q, want %q", in, got, want)
		}
	}
}
