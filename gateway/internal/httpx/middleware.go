// Package httpx contains HTTP plumbing shared by the gateway and admin listeners:
// request IDs, client IP resolution, panic recovery, access logging and security headers.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxClientIP
)

// HeaderRequestID is returned to clients so they can correlate with the request monitor.
const HeaderRequestID = "X-Request-Id"

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so that the first one is the outermost.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

var idEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// NewRequestID returns a 26-char random ID. Client-supplied IDs are never trusted.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return idEncoding.EncodeToString(b[:])
}

// RequestID assigns a fresh ID to every request and echoes it in the response.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := NewRequestID()
			w.Header().Set(HeaderRequestID, id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
		})
	}
}

// RequestIDFrom returns the request ID stored by RequestID, or "".
func RequestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(ctxRequestID).(string)
	return s
}

// ResolveClientIP stores the resolved client IP in the request context. The
// resolver is fetched per request so trusted-proxy settings can hot-reload.
func ResolveClientIP(get func() *ClientIPResolver) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := get().Resolve(r)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxClientIP, ip)))
		})
	}
}

// ClientIPFrom returns the ClientIP stored by ResolveClientIP.
func ClientIPFrom(ctx context.Context) ClientIP {
	ip, _ := ctx.Value(ctxClientIP).(ClientIP)
	return ip
}

// statusRecorder captures status and size while staying transparent to
// http.ResponseController (Flush for SSE, deadlines) via Unwrap.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Recover turns handler panics into a 500 and a logged stack trace.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					log.Error("panic in handler", "request_id", RequestIDFrom(r.Context()), "panic", v, "stack", string(debug.Stack()))
					WriteJSONError(w, http.StatusInternalServerError, "internal_error", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog writes one structured line per request. Query strings are redacted
// so gateway tokens passed as ?tavilyApiKey= never reach the logs.
func AccessLog(log *slog.Logger, surface string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			ip := ClientIPFrom(r.Context())
			level := slog.LevelInfo
			if r.URL.Path == "/healthz" {
				level = slog.LevelDebug
			}
			log.Log(r.Context(), level, "http",
				"surface", surface,
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"url", RedactURL(r.URL),
				"status", rec.status,
				"bytes", rec.bytes,
				"dur_ms", time.Since(start).Milliseconds(),
				"client_ip", ip.String(),
				"ip_source", ip.Source,
			)
		})
	}
}

// SecurityHeaders sets baseline headers. Admin pages get a strict CSP that
// forbids inline scripts and any third-party origin (ARCHITECTURE.md §11.4).
// Headers are Set (not Added) so an inner, more specific chain wins.
func SecurityHeaders(admin, hsts bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			if admin {
				h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
				h.Set("X-Frame-Options", "DENY")
				h.Set("Cross-Origin-Opener-Policy", "same-origin")
				h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
			} else {
				h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			}
			if hsts {
				h.Set("Strict-Transport-Security", "max-age=31536000")
			}
			next.ServeHTTP(w, r)
		})
	}
}
