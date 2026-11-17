package confstore

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v4"

	"github.com/sodiseng/search-warden/gateway/internal/auth/password"
	"github.com/sodiseng/search-warden/gateway/internal/auth/token"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/httpx"
)

// File names, relative to the data directory (always forward slashes).
const (
	FileConfig   = "config.yaml"
	FileEgresses = "egresses.yaml"
	FileTokens   = "tokens.yaml"
)

// Providers with a keys/<provider>.yaml file.
var Providers = []string{"tavily", "exa"}

// KeysFile returns the key file name of a provider.
func KeysFile(provider string) string { return "keys/" + provider + ".yaml" }

// AllFiles lists every managed file in a stable order.
func AllFiles() []string {
	files := []string{FileConfig, FileEgresses}
	for _, p := range Providers {
		files = append(files, KeysFile(p))
	}
	return append(files, FileTokens)
}

// KeyID derives the stable ID of an upstream key. Editing labels, order or
// egress in the file never changes it, so runtime state follows the key (§14.5).
func KeyID(provider, secret string) string {
	sum := sha256.Sum256([]byte(provider + ":" + secret))
	return hex.EncodeToString(sum[:6])
}

// ---- config.yaml --------------------------------------------------------

// Config is config.yaml (ARCHITECTURE.md §14.3).
type Config struct {
	Admin    AdminConfig
	Routing  RoutingConfig
	Upstream UpstreamConfig
	Quota    QuotaConfig
	Logs     LogsConfig
	Network  NetworkConfig
	MCP      MCPConfig
	Egress   EgressPolicy
}

type AdminConfig struct {
	Username           string
	PasswordHash       string
	TOTPSecret         string
	SessionIdleMinutes int
	SessionMaxDays     int

	plainPassword string // admin.password as written; rewritten to password_hash, never kept
}

type RoutingConfig struct {
	Strategy          string
	MaxAttempts       int
	DeadlineSeconds   int
	AffinityMinutes   int
	PerKeyRPM         int // requests per minute per upstream key; 0 = unlimited
	PerKeyConcurrency int // in-flight requests per upstream key; 0 = unlimited
}

type UpstreamConfig struct {
	PreciseBilling bool
	UserAgent      map[string]string // provider → UA; empty = built-in default
	ExtraHeaders   []string          // additional request headers allowed through to upstream
}

// DefaultUserAgent is sent upstream when upstream.user_agent is empty: one
// fixed, common client string, so a key never shows a different client from
// one call to the next (§7.7).
const DefaultUserAgent = "python-requests/2.32.3"

// UserAgentFor returns the User-Agent for every request to a provider,
// including quota syncs.
func (u UpstreamConfig) UserAgentFor(provider string) string {
	if ua := u.UserAgent[provider]; ua != "" {
		return ua
	}
	return DefaultUserAgent
}

// QuotaConfig is config.yaml quota.* (ARCHITECTURE.md §10.2).
type QuotaConfig struct {
	Sync               bool    // sync Tavily balances with GET /usage in the background
	HotIntervalMinutes int     // keys used in the last 2 hours
	ColdIntervalHours  int     // every other key
	ExaFreeMonthlyUSD  float64 // Exa's monthly free grant: the estimated balance of free keys without a budget
}

type LogsConfig struct {
	RetentionDays     int
	MaxSizeMB         int
	QueryPreviewChars int
}

type NetworkConfig struct {
	TrustedProxies  []string
	ClientIPHeaders []string
}

type MCPConfig struct {
	Modes map[string]string // provider → facade | passthrough
}

type EgressPolicy struct {
	MaxKeysPerEgress      int
	HealthIntervalSeconds int
}

var strategies = map[string]bool{"balanced": true, "drain-first": true, "round-robin": true, "random": true}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`)

func parseConfig(name string, d *Doc) (*Config, Errors) {
	r := &reader{file: name}
	top := r.fieldsOf(d.Top(), "")
	if d.Top() == nil && len(d.root.Content) > 0 && !isNull(d.root.Content[0]) {
		r.addf(d.root.Content[0], "top level must be a mapping")
	}
	c := &Config{}

	a := top.sub("admin")
	c.Admin.Username = a.str("username", "admin")
	if c.Admin.Username == "" || len(c.Admin.Username) > 64 || strings.ContainsFunc(c.Admin.Username, unicode.IsSpace) {
		r.addf(a.get("username"), "admin.username must be 1-64 characters without spaces")
	}
	c.Admin.PasswordHash = a.str("password_hash", "")
	if c.Admin.PasswordHash != "" && !password.IsHash(c.Admin.PasswordHash) {
		r.addf(a.get("password_hash"), "admin.password_hash is not a valid argon2id hash; write the new password as admin.password and it will be hashed automatically")
	}
	if pn := a.scalar("password"); pn != nil {
		c.Admin.plainPassword = pn.Value
		if err := password.Validate(pn.Value); err != nil {
			r.addf(pn, "admin.password: %v", err)
		}
	}
	c.Admin.TOTPSecret = strings.ToUpper(strings.ReplaceAll(a.str("totp_secret", ""), " ", ""))
	if s := c.Admin.TOTPSecret; s != "" {
		if _, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimRight(s, "=")); err != nil || len(s) < 16 {
			r.addf(a.get("totp_secret"), "admin.totp_secret must be a base32 secret of at least 16 characters (delete the line to disable TOTP)")
		}
	}
	c.Admin.SessionIdleMinutes = a.integer("session_idle_minutes", 60, 5, 7*24*60)
	c.Admin.SessionMaxDays = a.integer("session_max_days", 7, 1, 90)
	a.done()

	ro := top.sub("routing")
	c.Routing.Strategy = ro.str("strategy", "balanced")
	if !strategies[c.Routing.Strategy] {
		r.addf(ro.get("strategy"), "routing.strategy must be one of balanced, drain-first, round-robin, random")
	}
	c.Routing.MaxAttempts = ro.integer("max_attempts", 3, 1, 10)
	c.Routing.DeadlineSeconds = ro.integer("deadline_seconds", 180, 10, 1800)
	c.Routing.AffinityMinutes = ro.integer("affinity_minutes", 30, 0, 1440)
	c.Routing.PerKeyRPM = ro.integer("per_key_rpm", 0, 0, 100_000)
	c.Routing.PerKeyConcurrency = ro.integer("per_key_concurrency", 0, 0, 1000)
	ro.done()

	up := top.sub("upstream")
	c.Upstream.PreciseBilling = up.boolean("precise_billing", true)
	ua := up.sub("user_agent")
	c.Upstream.UserAgent = map[string]string{}
	for _, p := range Providers {
		v := ua.str(p, "")
		if len(v) > 256 || strings.ContainsAny(v, "\r\n") {
			r.addf(ua.get(p), "upstream.user_agent.%s must be a single line of at most 256 characters", p)
		}
		c.Upstream.UserAgent[p] = v
	}
	ua.done()
	if hs, ok := up.strList("extra_headers"); ok {
		for _, h := range hs {
			if !headerNameRe.MatchString(h) {
				r.addf(up.get("extra_headers"), "upstream.extra_headers: %q is not a valid header name", h)
			}
		}
		c.Upstream.ExtraHeaders = hs
	}
	up.done()

	qt := top.sub("quota")
	c.Quota.Sync = qt.boolean("sync", true)
	c.Quota.HotIntervalMinutes = qt.integer("hot_interval_minutes", 15, 5, 1440)
	c.Quota.ColdIntervalHours = qt.integer("cold_interval_hours", 24, 1, 720)
	c.Quota.ExaFreeMonthlyUSD = qt.number("exa_free_monthly_usd", 10, 0, 100000)
	qt.done()

	lg := top.sub("logs")
	c.Logs.RetentionDays = lg.integer("retention_days", 30, 1, 3650)
	c.Logs.MaxSizeMB = lg.integer("max_size_mb", 1024, 16, 1<<20)
	c.Logs.QueryPreviewChars = lg.integer("query_preview_chars", 40, 0, 200)
	lg.done()

	nw := top.sub("network")
	c.Network.TrustedProxies = httpx.DefaultTrustedProxies()
	if v, ok := nw.strList("trusted_proxies"); ok {
		c.Network.TrustedProxies = v
	}
	c.Network.ClientIPHeaders = httpx.DefaultClientIPHeaders()
	if v, ok := nw.strList("client_ip_headers"); ok {
		c.Network.ClientIPHeaders = v
	}
	if _, err := httpx.NewClientIPResolver(c.Network.TrustedProxies, c.Network.ClientIPHeaders); err != nil {
		r.addf(firstNonNil(nw.get("trusted_proxies"), nw.get("client_ip_headers")), "network: %v", err)
	}
	nw.done()

	mc := top.sub("mcp")
	c.MCP.Modes = map[string]string{}
	for _, p := range Providers {
		pf := mc.sub(p)
		mode := pf.str("mode", "facade")
		switch mode {
		case "facade":
		case "passthrough":
			r.addf(pf.get("mode"), "mcp.%s.mode: passthrough is planned for phase 2 and not available yet", p)
		default:
			r.addf(pf.get("mode"), "mcp.%s.mode must be facade", p)
		}
		c.MCP.Modes[p] = mode
		pf.done()
	}
	mc.done()

	eg := top.sub("egress")
	c.Egress.MaxKeysPerEgress = eg.integer("max_keys_per_egress", 0, 0, 1000)
	c.Egress.HealthIntervalSeconds = eg.integer("health_interval_seconds", 60, 10, 3600)
	eg.done()

	top.done()
	return c, r.errs
}

func firstNonNil(ns ...*yaml.Node) *yaml.Node {
	for _, n := range ns {
		if n != nil {
			return n
		}
	}
	return nil
}

// ---- egresses.yaml ------------------------------------------------------

// EgressFile is egresses.yaml.
type EgressFile struct {
	Nodes    []egress.Node
	Lines    map[string]int // lower-case name → line
	Registry *egress.Registry
}

func parseEgresses(name string, d *Doc) (*EgressFile, Errors) {
	r := &reader{file: name}
	top := r.fieldsOf(d.Top(), "")
	ef := &EgressFile{Lines: map[string]int{}}
	byURL := map[string]int{}
	for i, item := range top.seq("egresses") {
		f := r.fieldsOf(item, fmt.Sprintf("egresses[%d]", i))
		if f.m == nil {
			continue
		}
		n := egress.Node{
			Name:    f.str("name", ""),
			URL:     f.str("url", ""),
			Enabled: f.boolean("enabled", true),
			MaxKeys: f.integer("max_keys", 0, 0, 1000),
			Region:  f.str("region", ""),
			Note:    f.str("note", ""),
		}
		if tags, ok := f.strList("tags"); ok {
			n.Tags = tags
		}
		f.done()
		ok := true
		if err := egress.ValidateName(n.Name); err != nil {
			r.addf(firstNonNil(f.get("name"), item), "%v", err)
			ok = false
		}
		norm, err := egress.NormalizeURL(n.URL)
		if err != nil {
			r.addf(firstNonNil(f.get("url"), item), "%s: %v", n.Name, err)
			ok = false
		}
		if len(n.Region) > 32 || len(n.Tags) > 16 || len(n.Note) > 500 {
			r.addf(item, "%s: region (32), tags (16) or note (500) too long", n.Name)
		}
		if !ok {
			continue
		}
		lname := strings.ToLower(n.Name)
		if prev, dup := ef.Lines[lname]; dup {
			r.addf(item, "duplicate egress name %q (first defined on line %d)", n.Name, prev)
			continue
		}
		if prev, dup := byURL[norm]; dup {
			r.addf(item, "%s points to the same proxy as the node on line %d; one proxy can only have one name", n.Name, prev)
			continue
		}
		ef.Lines[lname] = item.Line
		byURL[norm] = item.Line
		ef.Nodes = append(ef.Nodes, n)
	}
	top.done()
	if len(r.errs) == 0 {
		reg, errs := egress.NewRegistry(ef.Nodes)
		for _, e := range errs {
			r.addf(nil, "%v", e)
		}
		ef.Registry = reg
	}
	return ef, r.errs
}

// ---- keys/<provider>.yaml -----------------------------------------------

// KeyItem is one parsed key entry before egress resolution.
type KeyItem struct {
	Secret        string
	Label         string
	Plan          string // free | paid
	EgressRef     string // as written: "", "direct", node name or URL
	Region        string
	Priority      int
	BudgetMonthly float64 // Tavily: credits; Exa: USD. 0 = no cap
	Team          string  // Exa: keys of one team share its rate limit
	RateLimit     float64 // overrides the main endpoint limit (Tavily RPM, Exa QPS); 0 = documented default
	Weight        float64 // share of traffic within its priority tier (default 1)
	ResetDay      int
	Enabled       bool
	Note          string
	Line          int
	EgressLine    int
}

// KeyFile is keys/<provider>.yaml.
type KeyFile struct {
	Provider string
	Items    []KeyItem
}

func parseKeys(name, provider string, d *Doc) (*KeyFile, Errors) {
	r := &reader{file: name}
	top := r.fieldsOf(d.Top(), "")
	df := top.sub("defaults")
	def := KeyItem{
		Plan:      df.str("plan", "free"),
		EgressRef: df.str("egress", egress.Direct),
		Region:    df.str("region", ""),
		Team:      df.str("team", ""),
		RateLimit: df.number("rate_limit", 0, 0, 1e6),
		Weight:    df.number("weight", 1, 0.001, 1000),
		ResetDay:  df.integer("reset_day", 1, 1, 28),
		Enabled:   df.boolean("enabled", true),
		Priority:  -1,
	}
	if df.has("priority") {
		def.Priority = df.integer("priority", 0, 0, 100)
	}
	if def.Plan != "free" && def.Plan != "paid" {
		r.addf(df.get("plan"), "defaults.plan must be free or paid")
	}
	if provider != "exa" && def.Team != "" {
		r.addf(df.get("team"), "defaults.team applies to Exa keys only (Exa rate limits are per team)")
		def.Team = ""
	}
	defEgressLine := 0
	if n := df.get("egress"); n != nil {
		defEgressLine = n.Line
	}
	df.done()

	kf := &KeyFile{Provider: provider}
	seen := map[string]int{}
	for i, item := range top.seq("keys") {
		k := def
		k.Line, k.EgressLine = item.Line, defEgressLine
		if k.EgressLine == 0 {
			k.EgressLine = item.Line
		}
		priorityExplicit := false
		switch item.Kind {
		case yaml.ScalarNode: // shorthand "- <key>"
			k.Secret = item.Value
		case yaml.MappingNode:
			f := r.fieldsOf(item, fmt.Sprintf("keys[%d]", i))
			k.Secret = f.str("key", "")
			k.Label = f.str("label", "")
			k.Plan = f.str("plan", def.Plan)
			if n := f.get("egress"); !isNull(n) {
				k.EgressRef, k.EgressLine = n.Value, n.Line
			}
			k.Region = f.str("region", def.Region)
			if f.has("priority") {
				k.Priority, priorityExplicit = f.integer("priority", 0, 0, 100), true
			}
			k.BudgetMonthly = f.number("budget_monthly", 0, 0, 1e12)
			k.Team = f.str("team", def.Team)
			k.RateLimit = f.number("rate_limit", def.RateLimit, 0, 1e6)
			k.Weight = f.number("weight", def.Weight, 0.001, 1000)
			if provider != "exa" && k.Team != "" {
				r.addf(f.get("team"), "keys[%d].team applies to Exa keys only (Exa rate limits are per team)", i)
			}
			k.ResetDay = f.integer("reset_day", def.ResetDay, 1, 28)
			k.Enabled = f.boolean("enabled", def.Enabled)
			k.Note = f.str("note", "")
			f.done()
			if k.Plan != "free" && k.Plan != "paid" {
				r.addf(f.get("plan"), "keys[%d].plan must be free or paid", i)
			}
		default:
			r.addf(item, "keys[%d] must be a key string or a mapping with a key field", i)
			continue
		}
		if !priorityExplicit && k.Priority < 0 {
			k.Priority = 0
			if k.Plan == "paid" {
				k.Priority = 1
			}
		}
		if err := validateSecret(provider, k.Secret); err != nil {
			r.addf(item, "keys[%d]: %v", i, err)
			continue
		}
		if provider == "tavily" && k.BudgetMonthly != math.Trunc(k.BudgetMonthly) {
			r.addf(item, "keys[%d].budget_monthly is in Tavily credits and must be a whole number", i)
		}
		if len(k.Label) > 64 || len(k.Note) > 500 || len(k.Region) > 32 || len(k.Team) > 64 {
			r.addf(item, "keys[%d]: label (64), team (64), note (500) or region (32) too long", i)
		}
		if prev, dup := seen[k.Secret]; dup {
			r.addf(item, "keys[%d] duplicates the key on line %d", i, prev)
			continue
		}
		seen[k.Secret] = item.Line
		kf.Items = append(kf.Items, k)
	}
	top.done()
	return kf, r.errs
}

func validateSecret(provider, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("key is empty")
	case len(s) > 512:
		return fmt.Errorf("key is too long")
	case strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return fmt.Errorf("key must not contain spaces")
	case provider == "tavily" && !strings.HasPrefix(s, "tvly-"):
		return fmt.Errorf("a Tavily key starts with \"tvly-\" (is this an Exa key? put it in keys/exa.yaml)")
	case provider != "tavily" && strings.HasPrefix(s, "tvly-"):
		return fmt.Errorf("this looks like a Tavily key; put it in keys/tavily.yaml")
	}
	return nil
}

// ---- tokens.yaml --------------------------------------------------------

// TokenLimits are per-token quotas; 0 means unlimited.
type TokenLimits struct {
	RPM             int
	Concurrency     int
	DailyRequests   int64
	MonthlyRequests int64
	MonthlyCredits  int64   // Tavily
	MonthlyUSD      float64 // Exa
}

// Token is one downstream access token (hash only).
type Token struct {
	ID        string // "t" + first 10 hex of the hash; stable across renames
	Name      string
	Hash      string
	Prefix    string
	Providers []string
	Surfaces  []string
	Tools     []string
	KeyGroup  string // all | free | paid
	IPAllow   []netip.Prefix
	Expires   time.Time // zero = never
	Enabled   bool
	Limits    TokenLimits
	Note      string
	Line      int

	plain string // "token:" as written; rewritten to token_hash, never kept
}

// TokenFile is tokens.yaml.
type TokenFile struct {
	Tokens []Token
}

var tokenNameRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N}._ -]{0,63}$`)

func parseTokens(name string, d *Doc) (*TokenFile, Errors) {
	r := &reader{file: name}
	top := r.fieldsOf(d.Top(), "")
	tf := &TokenFile{}
	names, hashes := map[string]int{}, map[string]int{}
	for i, item := range top.seq("tokens") {
		f := r.fieldsOf(item, fmt.Sprintf("tokens[%d]", i))
		if f.m == nil {
			continue
		}
		t := Token{
			Name:     f.str("name", ""),
			Hash:     strings.ToLower(f.str("token_hash", "")),
			Prefix:   f.str("token_prefix", ""),
			KeyGroup: f.str("key_group", "all"),
			Enabled:  f.boolean("enabled", true),
			Note:     f.str("note", ""),
			Line:     item.Line,
		}
		if pn := f.scalar("token"); pn != nil {
			t.plain = pn.Value
			if err := token.Validate(pn.Value); err != nil {
				r.addf(pn, "tokens[%d].token: %v", i, err)
			} else {
				t.Hash, t.Prefix = token.Hash(pn.Value), token.DisplayPrefix(pn.Value)
			}
		}
		t.Providers = listOrAll(r, f, "providers", Providers)
		t.Surfaces = listOrAll(r, f, "surfaces", []string{"mcp", "rest"})
		t.Tools, _ = f.strList("tools")
		if ips, ok := f.strList("ip_allowlist"); ok {
			for _, s := range ips {
				p, err := parsePrefix(s)
				if err != nil {
					r.addf(f.get("ip_allowlist"), "tokens[%d].ip_allowlist: %q is not an IP or CIDR", i, s)
					continue
				}
				t.IPAllow = append(t.IPAllow, p)
			}
		}
		if en := f.scalar("expires"); en != nil {
			ts, err := parseTime(en.Value)
			if err != nil {
				r.addf(en, "tokens[%d].expires must be a date like 2027-01-01 or RFC 3339 time", i)
			}
			t.Expires = ts
		}
		lf := f.sub("limits")
		t.Limits = TokenLimits{
			RPM:             lf.integer("rpm", 0, 0, 1_000_000),
			Concurrency:     lf.integer("concurrency", 0, 0, 10_000),
			DailyRequests:   int64(lf.integer("daily_requests", 0, 0, math.MaxInt32)),
			MonthlyRequests: int64(lf.integer("monthly_requests", 0, 0, math.MaxInt32)),
			MonthlyCredits:  int64(lf.integer("monthly_credits", 0, 0, math.MaxInt32)),
			MonthlyUSD:      lf.number("monthly_usd", 0, 0, 1e9),
		}
		lf.done()
		f.done()

		switch {
		case !tokenNameRe.MatchString(t.Name):
			r.addf(firstNonNil(f.get("name"), item), "tokens[%d].name is required (1-64 letters, digits, space, . _ -)", i)
			continue
		case t.Hash == "":
			r.addf(item, "tokens[%d] (%s) needs token_hash, or token: <plaintext> to be hashed automatically", i, t.Name)
			continue
		case !token.IsHash(t.Hash):
			r.addf(f.get("token_hash"), "tokens[%d].token_hash must be 64 hex characters", i)
			continue
		}
		if t.KeyGroup != "all" && t.KeyGroup != "free" && t.KeyGroup != "paid" {
			r.addf(f.get("key_group"), "tokens[%d].key_group must be all, free or paid", i)
		}
		lname := strings.ToLower(t.Name)
		if prev, dup := names[lname]; dup {
			r.addf(item, "duplicate token name %q (first defined on line %d)", t.Name, prev)
			continue
		}
		if prev, dup := hashes[t.Hash]; dup {
			r.addf(item, "token %q is the same token as the one on line %d", t.Name, prev)
			continue
		}
		names[lname], hashes[t.Hash] = item.Line, item.Line
		t.ID = "t" + t.Hash[:10]
		tf.Tokens = append(tf.Tokens, t)
	}
	top.done()
	return tf, r.errs
}

func listOrAll(r *reader, f *fields, key string, allowed []string) []string {
	vals, ok := f.strList(key)
	if !ok || len(vals) == 0 {
		return append([]string(nil), allowed...)
	}
	set := map[string]bool{}
	for _, a := range allowed {
		set[a] = true
	}
	var out []string
	for _, v := range vals {
		if !set[v] {
			r.addf(f.get(key), "%s: %q is not one of %s", f.p(key), v, strings.Join(allowed, ", "))
			continue
		}
		out = append(out, v)
	}
	return out
}

func parsePrefix(s string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q", s)
}
