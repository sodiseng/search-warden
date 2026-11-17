package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

type sink struct{ n int }

func (s *sink) Record(store.RequestLog, []store.Attempt) { s.n++ }

func get(h http.Handler, token string) (int, string) {
	r := httptest.NewRequest("GET", "/metrics", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Body)
	return w.Code, string(b)
}

func TestMetrics(t *testing.T) {
	next := &sink{}
	m := NewRecorder(next)
	m.Record(store.RequestLog{Provider: "tavily", Surface: "mcp", Status: 200, LatencyMS: 800, CostCredits: 2}, nil)
	m.Record(store.RequestLog{Provider: "tavily", Surface: "rest", Status: 429, LatencyMS: 40, ErrorKind: "rate_limited", Attempts: 3}, nil)
	m.Record(store.RequestLog{Provider: "exa", Surface: "mcp", Status: 200, LatencyMS: 3000, CostMicroUSD: 7000}, nil)
	if next.n != 3 {
		t.Fatal("records not passed on")
	}
	rem := int64(998)
	usd := int64(9_993_000)
	src := Sources{
		Keys: func() []pool.View {
			return []pool.View{{ID: "k1", Provider: "tavily", Label: `free"1`, Status: pool.StatusActive, Remaining: &rem, Unit: "credits"},
				{ID: "k2", Provider: "exa", Label: "e", Status: pool.StatusCooldown, Remaining: &usd, Unit: "micro_usd"}}
		},
		Egresses:   func() []outbound.View { return []outbound.View{{Label: "hk-1", Health: "quarantined", Enabled: true}} },
		LogDropped: func() int64 { return 4 },
		ConfigOK:   func() bool { return true },
		Version:    "v1",
	}
	h := Handler("secret-token", m, src)
	if code, _ := get(h, ""); code != 401 {
		t.Fatalf("no token = %d", code)
	}
	if code, _ := get(h, "wrong"); code != 401 {
		t.Fatalf("wrong token = %d", code)
	}
	if code, _ := get(Handler("", m, src), "x"); code != 404 {
		t.Fatalf("disabled = %d", code)
	}
	code, body := get(h, "secret-token")
	if code != 200 {
		t.Fatalf("scrape = %d", code)
	}
	for _, want := range []string{
		`warden_requests_total{provider="tavily",surface="mcp",status="2xx"} 1`,
		`warden_requests_total{provider="tavily",surface="rest",status="429"} 1`,
		`warden_request_duration_seconds_bucket{provider="tavily",le="1"} 2`,
		`warden_request_duration_seconds_bucket{provider="exa",le="2"} 0`,
		`warden_request_duration_seconds_bucket{provider="exa",le="+Inf"} 1`,
		`warden_upstream_credits_total{provider="tavily"} 2`,
		`warden_upstream_usd_total{provider="exa"} 0.007`,
		`warden_retries_total{provider="tavily"} 2`,
		`warden_request_errors_total{provider="tavily",kind="rate_limited"} 1`,
		`warden_keys{provider="exa",status="cooldown"} 1`,
		`warden_key_remaining{provider="tavily",key="k1",label="free\"1",unit="credits"} 998`,
		`warden_key_remaining{provider="exa",key="k2",label="e",unit="usd"} 9.993`,
		`warden_egress_up{egress="hk-1",health="quarantined"} 0`,
		`warden_log_dropped_total 4`,
		`warden_config_healthy 1`,
		`# TYPE warden_request_duration_seconds histogram`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}
