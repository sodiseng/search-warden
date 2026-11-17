package confstore

import (
	"strings"
	"testing"
)

func TestQuotaConfig(t *testing.T) {
	m, dir := openTest(t)
	if q := m.Snapshot().Config.Quota; !q.Sync || q.HotIntervalMinutes != 15 || q.ColdIntervalHours != 24 || q.ExaFreeMonthlyUSD != 10 {
		t.Fatalf("template values = %+v", q)
	}
	if _, err := m.SetConfig(nil, "quota.hot_interval_minutes", 1); err == nil {
		t.Fatal("a sync interval under 5 minutes was accepted")
	}
	if _, err := m.SetConfig(nil, "quota.sync", false); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Config.Quota.Sync {
		t.Fatal("quota.sync not applied")
	}

	// Configs written before the section existed get the same defaults.
	cfg := read(t, dir, FileConfig)
	start, end := strings.Index(cfg, "quota:\n"), strings.Index(cfg, "logs:\n")
	write(t, dir, FileConfig, cfg[:start]+cfg[end:])
	m.Reload()
	q := m.Snapshot().Config.Quota
	if !statusOf(m, FileConfig).Applied || !q.Sync || q.HotIntervalMinutes != 15 || q.ColdIntervalHours != 24 || q.ExaFreeMonthlyUSD != 10 {
		t.Fatalf("defaults = %+v, status %+v", q, statusOf(m, FileConfig))
	}
}

func TestUserAgentFor(t *testing.T) {
	u := UpstreamConfig{UserAgent: map[string]string{"exa": "my-agent/1.0", "tavily": ""}}
	if u.UserAgentFor("exa") != "my-agent/1.0" || u.UserAgentFor("tavily") != DefaultUserAgent {
		t.Fatalf("exa=%q tavily=%q", u.UserAgentFor("exa"), u.UserAgentFor("tavily"))
	}
}
