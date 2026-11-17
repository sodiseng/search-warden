//go:build uipreview

package admin

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/adminauth"
	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/egress"
	"github.com/sodiseng/search-warden/gateway/internal/httpx"
	"github.com/sodiseng/search-warden/gateway/internal/limits"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/retention"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// previewPassword signs in to the preview (user "admin").
const previewPassword = "preview-password-123"

// TestUIPreview serves the panel with made-up data until it is interrupted, so
// every page can be looked at (and screenshotted by scripts/ui-shots.cjs) without
// real keys, a real upstream or a running gateway. It sends the production
// security headers, so a page that breaks the CSP breaks here too.
//
//	go test -tags uipreview -run TestUIPreview -count=1 -timeout 0 ./internal/admin/
//
// It listens on WARDEN_PREVIEW_ADDR (default 127.0.0.1:18690).
func TestUIPreview(t *testing.T) {
	addr := os.Getenv("WARDEN_PREVIEW_ADDR")
	if addr == "" {
		addr = "127.0.0.1:18690"
	}
	ctx := context.Background()
	dir := t.TempDir()
	conf, err := confstore.Open(dir, confstore.Options{AdminPasswordEnv: previewPassword})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, config.DB{Driver: config.DriverSQLite, SQLiteDir: filepath.Join(dir, "db")})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	auth, err := adminauth.New(st, func() confstore.AdminConfig { return conf.Snapshot().Config.Admin }, adminauth.Options{BasePath: "/"})
	if err != nil {
		t.Fatal(err)
	}
	conf.Subscribe(auth.HandleConfigChange)

	nodes := "socks5://user:pass@10.20.0.11:1080 #hk-res-1\nsocks5://user:pass@10.20.0.12:1080 #sg-res-1\nhttp://user:pass@10.20.0.13:8080 #us-dc-1\n"
	if _, _, err := conf.ImportEgresses(nil, nodes, egress.ImportOptions{DefaultScheme: "socks5"}, "", []string{"residential"}, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conf.AddKeys(nil, []confstore.KeyInput{
		{Secret: "tvly-dev-demo0000000000000001", Label: "free-01", Plan: "free", Egress: "hk-res-1"},
		{Secret: "tvly-dev-demo0000000000000002", Label: "free-02", Plan: "free", Egress: "hk-res-1"},
		{Secret: "tvly-dev-demo0000000000000003", Label: "free-03", Plan: "free", Egress: "sg-res-1"},
		{Secret: "tvly-dev-demo0000000000000004", Label: "paid-01", Plan: "paid", Egress: "direct", Note: "production"},
		{Secret: "tvly-dev-demo0000000000000005", Label: "free-04", Plan: "free", Egress: "us-dc-1"},
		{Secret: "tvly-dev-demo0000000000000006", Label: "free-05", Plan: "free", Egress: "socks5://u:p@10.30.0.9:1080"},
		{Secret: "exa-demo-00000000000000000001", Label: "team-a-1", Plan: "free", Egress: "sg-res-1"},
		{Secret: "exa-demo-00000000000000000002", Label: "team-a-2", Plan: "free", Egress: "sg-res-1"},
		{Secret: "exa-demo-00000000000000000003", Label: "paid-exa", Plan: "paid", Egress: "direct"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []confstore.TokenSpec{
		{Name: "claude-laptop", Enabled: true, Note: "Claude Code on the laptop"},
		{Name: "codex-ci", Enabled: true, Surfaces: []string{"mcp"}, Tools: []string{"tavily_search", "web_search_exa"}, KeyGroup: "free",
			Limits: confstore.TokenLimits{RPM: 60, DailyRequests: 2000}},
		{Name: "cursor-team", Enabled: false, Expires: "2027-01-01", IPAllow: []string{"203.0.113.0/24"}, Limits: confstore.TokenLimits{MonthlyUSD: 5}},
	} {
		if _, _, err := conf.AddToken(nil, s); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now()
	snap := conf.Snapshot()
	i64 := func(v int64) *int64 { return &v }
	states := map[string]pool.View{
		"free-01":  {Status: pool.StatusActive, Limit: i64(1000), Used: i64(188), Remaining: i64(812), UpstreamPlan: "Researcher"},
		"free-02":  {Status: pool.StatusActive, Limit: i64(1000), Used: i64(769), Remaining: i64(231), UpstreamPlan: "Researcher"},
		"free-03":  {Status: pool.StatusCooldown, Until: now.Add(3 * time.Minute), Reason: "rate limited (429)", Limit: i64(1000), Used: i64(944), Remaining: i64(56)},
		"paid-01":  {Status: pool.StatusActive, Limit: i64(15000), Used: i64(5130), Remaining: i64(9870), UpstreamPlan: "Project"},
		"free-04":  {Status: pool.StatusEgressDown, SyncError: "egress us-dc-1 is quarantined"},
		"free-05":  {Status: pool.StatusInvalid, Reason: "401 invalid api key"},
		"team-a-1": {Status: pool.StatusActive, Team: "team-a", Unit: "micro_usd", SharedRemaining: true, LimitEstimated: true, Limit: i64(10_000_000), Used: i64(2_400_000), Remaining: i64(6_200_000)},
		"team-a-2": {Status: pool.StatusActive, Team: "team-a", Unit: "micro_usd", SharedRemaining: true, LimitEstimated: true, Limit: i64(10_000_000), Used: i64(1_400_000), Remaining: i64(6_200_000)},
		"paid-exa": {Status: pool.StatusExhausted, Unit: "micro_usd", Reason: "402 credits exhausted", Used: i64(48_250_000), Until: now.Add(9 * 24 * time.Hour)},
	}
	var views []pool.View
	keysOf := map[string][]string{}
	for i, k := range snap.Keys {
		v := states[k.Label]
		v.ID, v.Provider, v.Label, v.Plan = k.ID, k.Provider, k.DisplayName(), k.Plan
		v.PeriodResetAt = time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		v.LastSyncedAt = now.Add(-time.Duration(4+i*7) * time.Minute)
		if v.Status != pool.StatusInvalid && v.Status != pool.StatusEgressDown {
			v.LastUsedAt = now.Add(-time.Duration(1+i*i*9) * time.Minute)
		}
		views = append(views, v)
		keysOf[k.Provider] = append(keysOf[k.Provider], k.ID)
	}

	eviews := []outbound.View{{ID: egress.DirectID, Label: egress.Direct, Health: outbound.HealthHealthy, Enabled: true, LatencyMS: 38,
		ExitIP: "203.0.113.10", ExitCountry: "US", LastChecked: now.Add(-20 * time.Second)}}
	health := map[string]outbound.View{
		"hk-res-1": {Health: outbound.HealthHealthy, LatencyMS: 182, ExitIP: "198.51.100.21", ExitCountry: "HK"},
		"sg-res-1": {Health: "degraded", LatencyMS: 640, ExitIP: "198.51.100.34", ExitCountry: "SG", LastError: "probe took 640 ms"},
		"us-dc-1":  {Health: outbound.HealthQuarantined, LastError: "dial tcp 10.20.0.13:8080: i/o timeout", Until: now.Add(4 * time.Minute)},
	}
	for _, n := range snap.Egresses.Nodes() {
		v := health[n.Name]
		v.ID, v.Label, v.Enabled, v.LastChecked = egress.ID(n.URL), n.Name, n.Enabled, now.Add(-35*time.Second)
		eviews = append(eviews, v)
	}
	for _, k := range snap.Keys {
		if k.Egress.Inline { // the typed-in proxy shares hk-res-1's exit IP, which the page warns about
			eviews = append(eviews, outbound.View{ID: k.Egress.ID, Label: k.Egress.Label(), Health: outbound.HealthHealthy, Enabled: true,
				LatencyMS: 210, ExitIP: "198.51.100.21", ExitCountry: "HK", LastChecked: now.Add(-50 * time.Second)})
		}
	}

	rnd := rand.New(rand.NewSource(7))
	ops := map[string][]string{"tavily": {"search", "search", "search", "extract", "crawl", "map", "research"}, "exa": {"search", "search", "contents", "answer"}}
	ips := []string{"203.0.113.5", "203.0.113.5", "203.0.113.5", "203.0.113.77", "198.51.100.140", "192.0.2.61"}
	fails := []struct {
		status int
		kind   string
	}{{429, "rate_limited"}, {503, "no_keys"}, {502, "upstream_io"}, {504, "deadline"}, {400, "client_error"}, {502, "egress_connect"}}
	queries := []string{"golang html/template csp nonce", "tavily vs exa pricing 2026", "how to rotate api keys safely", "最新的 MCP 协议版本有哪些变化", "site:go.dev slog handler examples", ""}
	var logs []store.RequestLog
	var attempts []store.Attempt
	for i := 0; i < 900; i++ {
		age := time.Duration(rnd.Float64() * rnd.Float64() * float64(7*24*time.Hour)) // denser towards now
		prov := "tavily"
		if rnd.Intn(10) < 4 {
			prov = "exa"
		}
		l := store.RequestLog{TS: now.Add(-age), RequestID: fmt.Sprintf("req_%06x", i), Provider: prov, Surface: []string{"mcp", "mcp", "rest"}[rnd.Intn(3)],
			Operation: ops[prov][rnd.Intn(len(ops[prov]))], KeyID: keysOf[prov][rnd.Intn(len(keysOf[prov]))], TokenID: snap.Tokens[rnd.Intn(len(snap.Tokens))].ID,
			ClientIP: ips[rnd.Intn(len(ips))], IPSource: "X-Forwarded-For", UserAgent: "claude-code/2.1.316", EgressID: eviews[1+rnd.Intn(2)].ID, ExitIP: "198.51.100.21",
			Attempts: 1, Status: 200, Outcome: "ok", LatencyMS: 300 + rnd.Intn(2600), ReqBytes: int64(200 + rnd.Intn(600)), RespBytes: int64(2000 + rnd.Intn(40000)),
			QueryPreview: queries[rnd.Intn(len(queries))]}
		l.UpstreamLatencyMS = l.LatencyMS - 12
		if prov == "tavily" {
			l.CostCredits = int64(1 + rnd.Intn(2))
			if l.Operation == "research" {
				l.CostCredits, l.LatencyMS, l.CostEstimated = int64(4+rnd.Intn(12)), 20000+rnd.Intn(60000), true
			}
		} else {
			l.CostMicroUSD = int64(5000 + rnd.Intn(20000))
		}
		if rnd.Intn(100) < 7 {
			f := fails[rnd.Intn(len(fails))]
			l.Status, l.ErrorKind, l.Outcome, l.CostCredits, l.CostMicroUSD, l.Attempts = f.status, f.kind, "gateway_error", 0, 0, 2
			attempts = append(attempts, store.Attempt{RequestID: l.RequestID, N: 1, TS: l.TS, KeyID: l.KeyID, EgressID: l.EgressID, Status: 502, ErrorKind: "upstream_io", LatencyMS: 900},
				store.Attempt{RequestID: l.RequestID, N: 2, TS: l.TS.Add(time.Second), KeyID: l.KeyID, EgressID: l.EgressID, Status: f.status, ErrorKind: f.kind, LatencyMS: 400})
		}
		logs = append(logs, l)
	}
	if err := st.InsertRequestLogs(ctx, logs, attempts); err != nil {
		t.Fatal(err)
	}
	for _, a := range []store.AuditEntry{
		{Source: store.AuditPanel, IP: "203.0.113.5", Action: "login", Target: "admin"},
		{Source: store.AuditPanel, IP: "203.0.113.5", Action: "key.reveal", Target: "tavily free-01"},
		{Source: store.AuditFile, Action: "config.change", Target: "keys/tavily.yaml", Diff: "+ keys[5]: tvly-…0006 (free-05)\n~ keys[2].egress: hk-res-1 → sg-res-1"},
		{Source: store.AuditSystem, Action: "retention.run", Target: "request_logs", Diff: "deleted 1,204 rows older than 30 days"},
	} {
		if _, err := st.AppendAudit(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	p, err := New(Options{Auth: auth, Conf: conf, Store: st, BasePath: "/", GatewayURL: "https://gw.example.com",
		Keys:      func() []pool.View { return views },
		Egresses:  func() []outbound.View { return eviews },
		SyncQuota: func(ids ...string) int { return 1 },
		TestKeys:  func(ids ...string) int { return len(ids) },
		TokenUsage: func(string) limits.Usage {
			return limits.Usage{DayRequests: 132, MonthRequests: 2841, MonthCredits: 3120, MonthMicroUSD: 1_930_000, InFlight: 1}
		},
		LogStatus: func() retention.Status {
			return retention.Status{At: now.Add(-25 * time.Minute), SizeBytes: 41 << 20, MaxBytes: 512 << 20, Rows: 182_340, Oldest: now.Add(-29 * 24 * time.Hour), AgeDeleted: 1204}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := httpx.NewClientIPResolver(httpx.DefaultTrustedProxies(), httpx.DefaultClientIPHeaders())
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("panel preview: http://%s/  (user admin, password %s)\n", ln.Addr(), previewPassword)
	srv := &http.Server{Handler: httpx.Chain(p, httpx.ResolveClientIP(func() *httpx.ClientIPResolver { return res }), httpx.SecurityHeaders(true, false))}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
