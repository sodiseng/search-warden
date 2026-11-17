package confstore

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sodiseng/search-warden/gateway/internal/egress"
)

const maxSummaryLines = 50

// summarize describes what changed between two snapshots without revealing
// secrets (keys are shown by label/mask, proxy passwords are redacted).
func summarize(old, new *Snapshot, files []string) []string {
	if old == nil {
		return []string{fmt.Sprintf("loaded %d keys, %d egresses, %d tokens", len(new.Keys), len(new.Egresses.Nodes()), len(new.Tokens))}
	}
	var out []string
	touched := func(f string) bool { return slices.Contains(files, f) }
	if touched(FileConfig) {
		out = append(out, diffConfig(old.Config, new.Config)...)
	}
	if touched(FileEgresses) {
		out = append(out, diffEgresses(old.Egresses.Nodes(), new.Egresses.Nodes())...)
	}
	keysTouched := false
	for _, p := range Providers {
		keysTouched = keysTouched || touched(KeysFile(p))
	}
	if keysTouched || touched(FileEgresses) {
		out = append(out, diffKeys(old.Keys, new.Keys)...)
	}
	if touched(FileTokens) {
		out = append(out, diffTokens(old.Tokens, new.Tokens)...)
	}
	if len(out) == 0 {
		out = []string{"formatting or comments only"}
	}
	if len(out) > maxSummaryLines {
		n := len(out) - maxSummaryLines
		out = append(out[:maxSummaryLines], fmt.Sprintf("... and %d more", n))
	}
	return out
}

func diffConfig(a, b Config) []string {
	var out []string
	if a.Admin.PasswordHash != b.Admin.PasswordHash {
		out = append(out, "admin password changed")
	}
	switch {
	case a.Admin.TOTPSecret == "" && b.Admin.TOTPSecret != "":
		out = append(out, "admin TOTP enabled")
	case a.Admin.TOTPSecret != "" && b.Admin.TOTPSecret == "":
		out = append(out, "admin TOTP disabled")
	case a.Admin.TOTPSecret != b.Admin.TOTPSecret:
		out = append(out, "admin TOTP secret changed")
	}
	fa, fb := flattenConfig(a), flattenConfig(b)
	for _, k := range sortedKeys(fb) {
		if fa[k] != fb[k] {
			out = append(out, fmt.Sprintf("config %s: %s → %s", k, fa[k], fb[k]))
		}
	}
	return out
}

func flattenConfig(c Config) map[string]string {
	m := map[string]string{
		"admin.username":                 c.Admin.Username,
		"admin.session_idle_minutes":     fmt.Sprint(c.Admin.SessionIdleMinutes),
		"admin.session_max_days":         fmt.Sprint(c.Admin.SessionMaxDays),
		"routing.strategy":               c.Routing.Strategy,
		"routing.max_attempts":           fmt.Sprint(c.Routing.MaxAttempts),
		"routing.deadline_seconds":       fmt.Sprint(c.Routing.DeadlineSeconds),
		"routing.affinity_minutes":       fmt.Sprint(c.Routing.AffinityMinutes),
		"routing.per_key_rpm":            fmt.Sprint(c.Routing.PerKeyRPM),
		"routing.per_key_concurrency":    fmt.Sprint(c.Routing.PerKeyConcurrency),
		"upstream.precise_billing":       fmt.Sprint(c.Upstream.PreciseBilling),
		"upstream.extra_headers":         fmt.Sprint(c.Upstream.ExtraHeaders),
		"logs.retention_days":            fmt.Sprint(c.Logs.RetentionDays),
		"logs.max_size_mb":               fmt.Sprint(c.Logs.MaxSizeMB),
		"logs.query_preview_chars":       fmt.Sprint(c.Logs.QueryPreviewChars),
		"network.trusted_proxies":        fmt.Sprint(c.Network.TrustedProxies),
		"network.client_ip_headers":      fmt.Sprint(c.Network.ClientIPHeaders),
		"egress.max_keys_per_egress":     fmt.Sprint(c.Egress.MaxKeysPerEgress),
		"egress.health_interval_seconds": fmt.Sprint(c.Egress.HealthIntervalSeconds),
	}
	for p, ua := range c.Upstream.UserAgent {
		m["upstream.user_agent."+p] = fmt.Sprintf("%q", ua)
	}
	for p, mode := range c.MCP.Modes {
		m["mcp."+p+".mode"] = mode
	}
	return m
}

func diffEgresses(a, b []egress.Node) []string {
	am, bm := map[string]egress.Node{}, map[string]egress.Node{}
	for _, n := range a {
		am[strings.ToLower(n.Name)] = n
	}
	for _, n := range b {
		bm[strings.ToLower(n.Name)] = n
	}
	var out []string
	for _, n := range b {
		o, ok := am[strings.ToLower(n.Name)]
		if !ok {
			out = append(out, fmt.Sprintf("+egress %s (%s)", n.Name, egress.Redact(n.URL)))
			continue
		}
		var ch []string
		if o.URL != n.URL {
			ch = append(ch, fmt.Sprintf("url %s → %s", egress.Redact(o.URL), egress.Redact(n.URL)))
		}
		if o.Enabled != n.Enabled {
			ch = append(ch, fmt.Sprintf("enabled %v → %v", o.Enabled, n.Enabled))
		}
		if o.MaxKeys != n.MaxKeys || o.Region != n.Region || o.Note != n.Note || !slices.Equal(o.Tags, n.Tags) {
			ch = append(ch, "details")
		}
		if len(ch) > 0 {
			out = append(out, fmt.Sprintf("~egress %s: %s", n.Name, strings.Join(ch, ", ")))
		}
	}
	for _, n := range a {
		if _, ok := bm[strings.ToLower(n.Name)]; !ok {
			out = append(out, "-egress "+n.Name)
		}
	}
	return out
}

func diffKeys(a, b []Key) []string {
	am := map[string]Key{}
	for _, k := range a {
		am[k.ID] = k
	}
	bm := map[string]bool{}
	var out []string
	for _, k := range b {
		bm[k.ID] = true
		o, ok := am[k.ID]
		if !ok {
			out = append(out, fmt.Sprintf("+key %s %s", k.Provider, k.DisplayName()))
			continue
		}
		var ch []string
		if o.Egress.ID != k.Egress.ID || o.EgressRef != k.EgressRef {
			ch = append(ch, fmt.Sprintf("egress %s → %s", o.Egress.Label(), k.Egress.Label()))
		}
		if o.Enabled != k.Enabled {
			ch = append(ch, fmt.Sprintf("enabled %v → %v", o.Enabled, k.Enabled))
		}
		if o.Label != k.Label {
			ch = append(ch, fmt.Sprintf("label %q → %q", o.Label, k.Label))
		}
		if o.Plan != k.Plan || o.Priority != k.Priority || o.BudgetMonthly != k.BudgetMonthly || o.ResetDay != k.ResetDay {
			ch = append(ch, "plan/priority/budget")
		}
		if o.Region != k.Region || o.Note != k.Note || o.Team != k.Team || o.RateLimit != k.RateLimit || o.Weight != k.Weight {
			ch = append(ch, "details")
		}
		if len(ch) > 0 {
			out = append(out, fmt.Sprintf("~key %s %s: %s", k.Provider, k.DisplayName(), strings.Join(ch, ", ")))
		}
	}
	for _, k := range a {
		if !bm[k.ID] {
			out = append(out, fmt.Sprintf("-key %s %s", k.Provider, k.DisplayName()))
		}
	}
	return out
}

func diffTokens(a, b []Token) []string {
	am := map[string]Token{}
	for _, t := range a {
		am[t.ID] = t
	}
	bm := map[string]bool{}
	var out []string
	for _, t := range b {
		bm[t.ID] = true
		o, ok := am[t.ID]
		if !ok {
			out = append(out, fmt.Sprintf("+token %s (%s…)", t.Name, t.Prefix))
			continue
		}
		if o.Name != t.Name || o.Enabled != t.Enabled || o.KeyGroup != t.KeyGroup || o.Limits != t.Limits ||
			!o.Expires.Equal(t.Expires) || !slices.Equal(o.Providers, t.Providers) || !slices.Equal(o.Surfaces, t.Surfaces) ||
			!slices.Equal(o.Tools, t.Tools) || !slices.Equal(o.IPAllow, t.IPAllow) || o.Note != t.Note {
			out = append(out, "~token "+t.Name)
		}
	}
	for _, t := range a {
		if !bm[t.ID] {
			out = append(out, "-token "+t.Name)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
