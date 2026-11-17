// Package adminauth authenticates the single admin account of the panel
// (ARCHITECTURE.md §11.1): argon2id password + optional TOTP, server-side
// sessions stored as SHA-256 hashes, CSRF tokens bound to the session,
// Origin checks, login throttling, and revocation of every session whenever
// the admin credentials change (from the panel or by editing config.yaml).
package adminauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/auth/password"
	"github.com/sodiseng/search-warden/gateway/internal/auth/totp"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// ErrInvalidCredentials is the only error a failed login reveals: it never
// says whether the username, password or code was wrong.
var ErrInvalidCredentials = errors.New("invalid username, password or verification code")

// LockedError means login attempts are temporarily blocked.
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string {
	return fmt.Sprintf("too many login attempts; try again in %s", e.RetryAfter.Round(time.Second))
}

// Store is the subset of *store.Store used here.
type Store interface {
	CreateSession(ctx context.Context, s store.Session) error
	GetSession(ctx context.Context, idHash string, now time.Time) (store.Session, error)
	TouchSession(ctx context.Context, idHash string, seen time.Time) error
	ListSessions(ctx context.Context, now time.Time) ([]store.Session, error)
	DeleteSession(ctx context.Context, idHash string) error
	DeleteAllSessions(ctx context.Context) (int64, error)
	DeleteExpiredSessions(ctx context.Context, now, idleCutoff time.Time) (int64, error)
	AppendAudit(ctx context.Context, e store.AuditEntry) (int64, error)
}

// Options configures cookies and Origin checks.
type Options struct {
	CookieSecure bool
	BasePath     string // admin base path, "/" or e.g. "/admin/"
	PublicURL    string // WARDEN_ADMIN_PUBLIC_URL; when set, Origin must match it
	Logger       *slog.Logger
	Now          func() time.Time
}

// Session is an authenticated admin session.
type Session struct {
	IDHash     string
	CSRF       string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	IP         string
	UserAgent  string
}

// Auth is safe for concurrent use.
type Auth struct {
	st    Store
	admin func() confstore.AdminConfig
	opts  Options
	log   *slog.Logger
	lim   *limiter

	totpMu   sync.Mutex
	totpLast uint64 // last accepted TOTP counter (replay protection)

	// dummyHash keeps the timing of a wrong username equal to a wrong password.
	dummyHash string
}

// touchEvery limits last-seen writes to one per minute per session.
const touchEvery = time.Minute

// New creates an Auth. admin returns the live admin config (from confstore).
func New(st Store, admin func() confstore.AdminConfig, opts Options) (*Auth, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BasePath == "" {
		opts.BasePath = "/"
	}
	dummy, err := password.Hash("dummy-password-for-timing")
	if err != nil {
		return nil, err
	}
	return &Auth{st: st, admin: admin, opts: opts, log: opts.Logger, lim: newLimiter(), dummyHash: dummy}, nil
}

// CookieName uses the __Host- prefix when possible (Secure, Path=/, no Domain),
// which stops subdomains from planting or overwriting the session cookie.
func (a *Auth) CookieName() string {
	if a.opts.CookieSecure && a.opts.BasePath == "/" {
		return "__Host-warden_admin"
	}
	return "warden_admin"
}

// TOTPEnabled reports whether a verification code is required at login.
func (a *Auth) TOTPEnabled() bool { return a.admin().TOTPSecret != "" }

func hashID(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func csrfFor(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("warden-csrf-v1"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Login checks throttling, then credentials, and on success creates a session.
func (a *Auth) Login(ctx context.Context, ip, ua, username, pw, code string) (*http.Cookie, *Session, error) {
	now := a.opts.Now()
	if wait, ok := a.lim.allow(ip, now); !ok {
		a.audit(ctx, ip, "admin.login_throttled", username, "")
		return nil, nil, &LockedError{RetryAfter: wait}
	}
	cfg := a.admin()
	userOK := subtle.ConstantTimeCompare(hashBytes(username), hashBytes(cfg.Username)) == 1
	hash := cfg.PasswordHash
	if hash == "" {
		hash = a.dummyHash
	}
	pwOK, err := password.Verify(hash, pw)
	if err != nil {
		a.log.Error("admin password hash unusable", "err", err)
		pwOK = false
	}
	ok := userOK && pwOK && cfg.PasswordHash != ""
	if ok && cfg.TOTPSecret != "" {
		ok = a.checkTOTP(cfg.TOTPSecret, code, now)
	}
	a.lim.record(ip, now, ok)
	if !ok {
		a.audit(ctx, ip, "admin.login_failed", username, "")
		return nil, nil, ErrInvalidCredentials
	}
	c, s, err := a.NewSession(ctx, ip, ua)
	if err != nil {
		return nil, nil, err
	}
	a.audit(ctx, ip, "admin.login", cfg.Username, "")
	return c, s, nil
}

func hashBytes(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func (a *Auth) checkTOTP(secret, code string, now time.Time) bool {
	a.totpMu.Lock()
	defer a.totpMu.Unlock()
	c, ok := totp.Verify(secret, code, now, a.totpLast)
	if ok {
		a.totpLast = c
	}
	return ok
}

// NewSession creates a session for an already-authenticated admin (login, or
// re-login right after the current user changed the password).
func (a *Auth) NewSession(ctx context.Context, ip, ua string) (*http.Cookie, *Session, error) {
	secret, err := newSecret()
	if err != nil {
		return nil, nil, err
	}
	now := a.opts.Now().Truncate(time.Millisecond)
	cfg := a.admin()
	s := &Session{
		IDHash: hashID(secret), CSRF: csrfFor(secret),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Duration(cfg.SessionMaxDays) * 24 * time.Hour),
		IP: ip, UserAgent: truncate(ua, 200),
	}
	if err := a.st.CreateSession(ctx, store.Session{IDHash: s.IDHash, CreatedAt: s.CreatedAt, LastSeenAt: s.LastSeenAt,
		ExpiresAt: s.ExpiresAt, IP: s.IP, UserAgent: s.UserAgent}); err != nil {
		return nil, nil, err
	}
	return a.cookie(secret, s.ExpiresAt), s, nil
}

func (a *Auth) cookie(value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name: a.CookieName(), Value: value, Path: a.opts.BasePath,
		Expires: expires, HttpOnly: true, Secure: a.opts.CookieSecure, SameSite: http.SameSiteStrictMode,
	}
}

// ClearCookie deletes the session cookie in the browser.
func (a *Auth) ClearCookie(w http.ResponseWriter) {
	c := a.cookie("", time.Unix(0, 0))
	c.MaxAge = -1
	http.SetCookie(w, c)
}

// Authenticate returns the session for r, or nil when there is none or it has
// expired (absolute or idle). Idle sessions are deleted on sight.
func (a *Auth) Authenticate(r *http.Request) (*Session, error) {
	c, err := r.Cookie(a.CookieName())
	if err != nil || c.Value == "" || len(c.Value) > 100 {
		return nil, nil
	}
	ctx := r.Context()
	now := a.opts.Now()
	id := hashID(c.Value)
	rec, err := a.st.GetSession(ctx, id, now)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	idle := time.Duration(a.admin().SessionIdleMinutes) * time.Minute
	if now.Sub(rec.LastSeenAt) > idle {
		_ = a.st.DeleteSession(ctx, id)
		return nil, nil
	}
	if now.Sub(rec.LastSeenAt) > touchEvery {
		if err := a.st.TouchSession(ctx, id, now); err != nil {
			a.log.Warn("session touch failed", "err", err)
		}
		rec.LastSeenAt = now
	}
	return &Session{IDHash: id, CSRF: csrfFor(c.Value), CreatedAt: rec.CreatedAt, LastSeenAt: rec.LastSeenAt,
		ExpiresAt: rec.ExpiresAt, IP: rec.IP, UserAgent: rec.UserAgent}, nil
}

// Logout deletes the session and its cookie.
func (a *Auth) Logout(ctx context.Context, w http.ResponseWriter, s *Session, ip string) error {
	a.ClearCookie(w)
	if s == nil {
		return nil
	}
	a.audit(ctx, ip, "admin.logout", "", "")
	return a.st.DeleteSession(ctx, s.IDHash)
}

// Sessions lists live sessions, newest first.
func (a *Auth) Sessions(ctx context.Context) ([]store.Session, error) {
	return a.st.ListSessions(ctx, a.opts.Now())
}

// Revoke deletes one session by its ID hash.
func (a *Auth) Revoke(ctx context.Context, idHash, ip string) error {
	a.audit(ctx, ip, "admin.session_revoked", shortID(idHash), "")
	return a.st.DeleteSession(ctx, idHash)
}

// RevokeAll deletes every session.
func (a *Auth) RevokeAll(ctx context.Context, ip, reason string) (int64, error) {
	n, err := a.st.DeleteAllSessions(ctx)
	if err == nil && n > 0 {
		a.audit(ctx, ip, "admin.sessions_revoked", "", fmt.Sprintf("%d session(s): %s", n, reason))
		a.log.Info("admin sessions revoked", "count", n, "reason", reason)
	}
	return n, err
}

// VerifyPassword re-checks the current password before sensitive changes.
func (a *Auth) VerifyPassword(pw string) bool {
	ok, err := password.Verify(a.admin().PasswordHash, pw)
	return err == nil && ok
}

// CheckPassword re-verifies the current password for a sensitive change, with
// the same throttling as logins (a stolen session must not allow guessing).
func (a *Auth) CheckPassword(ctx context.Context, ip, pw string) error {
	now := a.opts.Now()
	if wait, ok := a.lim.allow(ip, now); !ok {
		return &LockedError{RetryAfter: wait}
	}
	ok := a.VerifyPassword(pw)
	a.lim.record(ip, now, ok)
	if !ok {
		a.audit(ctx, ip, "admin.reauth_failed", "", "")
		return ErrInvalidCredentials
	}
	return nil
}

// CheckTOTP verifies a code against the configured secret, with replay protection.
func (a *Auth) CheckTOTP(code string) bool {
	secret := a.admin().TOTPSecret
	return secret != "" && a.checkTOTP(secret, code, a.opts.Now())
}

// HandleConfigChange revokes every session when the admin username, password
// or TOTP secret changed — whether from the panel or a hand edit of config.yaml.
func (a *Auth) HandleConfigChange(c confstore.Change) {
	if c.Old == nil {
		return
	}
	o, n := c.Old.Config.Admin, c.New.Config.Admin
	if o.TOTPSecret != n.TOTPSecret {
		a.totpMu.Lock()
		a.totpLast = 0
		a.totpMu.Unlock()
	}
	if o.PasswordHash == n.PasswordHash && o.TOTPSecret == n.TOTPSecret && o.Username == n.Username {
		return
	}
	if _, err := a.RevokeAll(context.Background(), "", "admin credentials changed via "+c.Source); err != nil {
		a.log.Error("could not revoke sessions after credential change", "err", err)
	}
}

// Cleanup removes expired sessions and stale throttling state. Run it periodically.
func (a *Auth) Cleanup(ctx context.Context) {
	now := a.opts.Now()
	idle := time.Duration(a.admin().SessionIdleMinutes) * time.Minute
	if _, err := a.st.DeleteExpiredSessions(ctx, now, now.Add(-idle)); err != nil {
		a.log.Warn("session cleanup failed", "err", err)
	}
	a.lim.gc(now)
}

func (a *Auth) audit(ctx context.Context, ip, action, target, diff string) {
	if _, err := a.st.AppendAudit(ctx, store.AuditEntry{Source: store.AuditPanel, IP: ip, Action: action,
		Target: truncate(target, 64), Diff: diff}); err != nil {
		a.log.Warn("audit write failed", "action", action, "err", err)
	}
}

func shortID(idHash string) string {
	if len(idHash) > 8 {
		return idHash[:8]
	}
	return idHash
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
