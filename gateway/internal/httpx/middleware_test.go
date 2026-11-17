package httpx

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDUniqueAndExposed(t *testing.T) {
	var seen string
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}), RequestID())

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderRequestID, "client-chosen")
	h.ServeHTTP(rec, r)

	got := rec.Header().Get(HeaderRequestID)
	if len(got) != 26 || got != seen || got == "client-chosen" {
		t.Fatalf("request id %q (ctx %q)", got, seen)
	}
	if a, b := NewRequestID(), NewRequestID(); a == b {
		t.Fatal("request IDs must be random")
	}
}

func TestAccessLogRedactsTokensAndRecordsClientIP(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	res, _ := NewClientIPResolver(DefaultTrustedProxies(), DefaultClientIPHeaders())
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("x"))
	}), RequestID(), ResolveClientIP(func() *ClientIPResolver { return res }), AccessLog(log, "gateway"))

	r := httptest.NewRequest(http.MethodPost, "/tavily/mcp?tavilyApiKey=sw_SUPERSECRET&x=1", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.77")
	h.ServeHTTP(httptest.NewRecorder(), r)

	out := buf.String()
	if strings.Contains(out, "SUPERSECRET") {
		t.Fatalf("token leaked into access log: %s", out)
	}
	for _, want := range []string{`"status":418`, `"client_ip":"198.51.100.77"`, `"ip_source":"X-Forwarded-For"`, `"bytes":1`, `x=1`} {
		if !strings.Contains(out, want) {
			t.Errorf("access log missing %s: %s", want, out)
		}
	}
}

func TestRecoverReturns500(t *testing.T) {
	var buf bytes.Buffer
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
		Recover(slog.New(slog.NewTextHandler(&buf, nil))))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(buf.String(), "boom") {
		t.Fatalf("code=%d log=%s", rec.Code, buf.String())
	}
}

func TestSecurityHeaders(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	rec := httptest.NewRecorder()
	Chain(ok, SecurityHeaders(true, true)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
		t.Errorf("admin CSP = %q", csp)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Strict-Transport-Security") == "" {
		t.Errorf("admin headers = %v", rec.Header())
	}

	rec = httptest.NewRecorder()
	Chain(ok, SecurityHeaders(false, false)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Strict-Transport-Security") != "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("gateway headers = %v", rec.Header())
	}
}

func TestStatusRecorderSupportsFlush(t *testing.T) {
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through middleware failed: %v", err)
		}
	}), AccessLog(slog.New(slog.DiscardHandler), "gateway"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}
