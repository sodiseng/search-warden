package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sodiseng/search-warden/gateway/internal/config"
)

func TestMetricsEndpoint(t *testing.T) {
	const tok = "metrics-token-0123456789"
	// Default: the panel shares the gateway port, so /metrics lives there too.
	a := mustApp(t, map[string]string{"WARDEN_METRICS_TOKEN": tok})
	if a.Admin != nil {
		t.Fatal("the panel is on the gateway listener by default")
	}
	if rec := do(a.Gateway, http.MethodGet, "/metrics"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", rec.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	a.Gateway.ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "warden_build_info") {
		t.Fatalf("scrape = %d %s", rec.Code, rec.Body.String())
	}
	// Disabled without a token.
	if rec := do(mustApp(t, nil).Gateway, http.MethodGet, "/metrics"); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled = %d", rec.Code)
	}
	// With a separate panel port the endpoint moves to the admin listener.
	sep := mustApp(t, map[string]string{"WARDEN_METRICS_TOKEN": tok, "WARDEN_ADMIN_LISTEN": ":18684"})
	if rec := do(sep.Admin, http.MethodGet, "/metrics"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("separate admin /metrics = %d", rec.Code)
	}
	if rec := do(sep.Gateway, http.MethodGet, "/metrics"); rec.Code != http.StatusNotFound {
		t.Fatalf("gateway port in separate mode = %d", rec.Code)
	}
	if _, err := config.Load(func(k string) string {
		if k == "WARDEN_METRICS_TOKEN" {
			return "short"
		}
		return ""
	}); err == nil {
		t.Fatal("short metrics token accepted")
	}
}
