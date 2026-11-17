// Package app wires configuration, listeners and handlers, and owns the process lifecycle.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/admin"
	"github.com/sodiseng/search-warden/gateway/internal/auth/adminauth"
	"github.com/sodiseng/search-warden/gateway/internal/buildinfo"
	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/httpx"
	"github.com/sodiseng/search-warden/gateway/internal/limits"
	"github.com/sodiseng/search-warden/gateway/internal/mcp"
	"github.com/sodiseng/search-warden/gateway/internal/metrics"
	"github.com/sodiseng/search-warden/gateway/internal/obs"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/quota"
	"github.com/sodiseng/search-warden/gateway/internal/relay"
	"github.com/sodiseng/search-warden/gateway/internal/retention"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

// ShutdownTimeout bounds graceful shutdown; long MCP streams are cut after it.
const ShutdownTimeout = 15 * time.Second

// Deps are the long-lived services the handlers use.
type Deps struct {
	Conf  *confstore.Manager
	Store *store.Store
	Pool  *pool.Pool        // loaded and applied by the caller (main)
	Out   *outbound.Runtime // loaded and applied by the caller (main)
	Rec   *obs.Writer
	// Limits enforces token limits (loaded by the caller); nil = a fresh,
	// unpersisted limiter.
	Limits *limits.Limiter
}

// App is the assembled process. Handlers are exported for tests.
type App struct {
	cfg     config.Config
	log     *slog.Logger
	deps    Deps
	auth    *adminauth.Auth
	quota   *quota.Syncer
	metrics *metrics.Recorder
	keep    *retention.Job
	ipRes   atomic.Pointer[httpx.ClientIPResolver]
	Gateway http.Handler // served on cfg.Listen (includes admin when AdminShared)
	Admin   http.Handler // served on cfg.AdminListen; nil when AdminShared
}

// New builds handlers and subscribes to config changes. It performs no I/O.
func New(cfg config.Config, log *slog.Logger, deps Deps) (*App, error) {
	if deps.Conf == nil || deps.Store == nil || deps.Pool == nil || deps.Out == nil || deps.Rec == nil {
		return nil, errors.New("app: Conf, Store, Pool, Out and Rec are required")
	}
	if deps.Limits == nil {
		deps.Limits = limits.New(nil, log, nil)
	}
	a := &App{cfg: cfg, log: log, deps: deps}
	if err := a.applyNetwork(); err != nil {
		return nil, err
	}
	auth, err := adminauth.New(deps.Store, func() confstore.AdminConfig { return deps.Conf.Snapshot().Config.Admin },
		adminauth.Options{CookieSecure: cfg.CookieSecure, BasePath: cfg.AdminBasePath, PublicURL: cfg.AdminPublicURL, Logger: log})
	if err != nil {
		return nil, err
	}
	a.auth = auth
	deps.Conf.Subscribe(func(c confstore.Change) {
		if err := a.applyNetwork(); err != nil {
			log.Error("network settings not applied", "err", err) // unreachable: confstore validated them
		}
		auth.HandleConfigChange(c) // revoke sessions when admin credentials change
		deps.Out.Apply(c.New)      // per-key connections follow egress changes
		deps.Pool.Apply(c.New)     // keys added/removed/edited keep their runtime state
		// Every config change — panel, hand edit or automatic — lands in the audit log (secrets redacted).
		if _, err := deps.Store.AppendAudit(context.Background(), store.AuditEntry{Source: c.Source, Action: "config.change",
			Target: strings.Join(c.Files, ","), Diff: strings.Join(c.Summary, "\n")}); err != nil {
			log.Warn("audit write failed", "err", err)
		}
	})
	a.quota = quota.New(quota.Options{Snapshot: deps.Conf.Snapshot, Pool: deps.Pool, Out: deps.Out, Logger: log})
	a.keep = retention.New(retention.Options{Store: deps.Store, Snapshot: deps.Conf.Snapshot, Logger: log})
	panel, err := admin.New(admin.Options{Auth: auth, Conf: deps.Conf, Store: deps.Store, BasePath: cfg.AdminBasePath, GatewayURL: cfg.PublicURL,
		EnvPassword: cfg.AdminPassword != "", Keys: deps.Pool.Views, Egresses: deps.Out.Views, ProbeEgress: deps.Out.ProbeNow,
		SyncQuota: a.quota.Request, TestKeys: a.quota.Test, TokenUsage: deps.Limits.Usage, LogStatus: a.keep.Status, Logger: log})
	if err != nil {
		return nil, err
	}

	common := func(surface string, admin bool) []httpx.Middleware {
		return []httpx.Middleware{
			httpx.RequestID(),
			httpx.ResolveClientIP(a.ipRes.Load),
			httpx.AccessLog(log, surface),
			httpx.Recover(log),
			httpx.SecurityHeaders(admin, admin && cfg.CookieSecure),
		}
	}

	var adminInner http.Handler = panel
	a.metrics = metrics.NewRecorder(deps.Rec)
	rl := relay.New(relay.Options{Conf: deps.Conf, Pool: deps.Pool, Out: deps.Out, Rec: a.metrics, Limits: deps.Limits, Logger: log})
	metricsH := metrics.Handler(cfg.MetricsToken, a.metrics, metrics.Sources{Keys: deps.Pool.Views, Egresses: deps.Out.Views,
		LogDropped: deps.Rec.Dropped, ConfigOK: deps.Conf.Healthy, Version: buildinfo.Version})
	mcpSrv, err := mcp.New(mcp.Options{Conf: deps.Conf, Relay: rl, Logger: log})
	if err != nil {
		return nil, err
	}
	gw := a.gatewayMux(rl, mcpSrv)
	if cfg.AdminShared {
		gw.Handle("GET /metrics", metricsH)
		// Admin chain nests inside the gateway chain; its SecurityHeaders run last and win.
		prefix := strings.TrimSuffix(cfg.AdminBasePath, "/")
		gw.Handle(cfg.AdminBasePath, httpx.Chain(http.StripPrefix(prefix, adminInner),
			httpx.SecurityHeaders(true, cfg.CookieSecure)))
		// The catch-all "/" would otherwise swallow the slash-less base path.
		gw.Handle(prefix, http.RedirectHandler(cfg.AdminBasePath, http.StatusMovedPermanently))
	} else {
		m := http.NewServeMux()
		m.Handle("GET /healthz", a.healthHandler())
		m.Handle("GET /metrics", metricsH)
		if cfg.AdminBasePath == "/" {
			m.Handle("/", adminInner)
		} else {
			m.Handle(cfg.AdminBasePath, http.StripPrefix(strings.TrimSuffix(cfg.AdminBasePath, "/"), adminInner))
			m.HandleFunc("/{$}", bareDomain)
		}
		a.Admin = httpx.Chain(m, common("admin", true)...)
	}
	a.Gateway = httpx.Chain(gw, common("gateway", false)...)
	return a, nil
}

// applyNetwork (re)builds the client IP resolver from config.yaml network.*.
func (a *App) applyNetwork() error {
	n := a.deps.Conf.Snapshot().Config.Network
	res, err := httpx.NewClientIPResolver(n.TrustedProxies, n.ClientIPHeaders)
	if err != nil {
		return err
	}
	a.ipRes.Store(res)
	return nil
}

// healthHandler stays 200 when a config file has a rejected edit ("degraded"):
// the last good config is still serving, so the container must not be restarted.
func (a *App) healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := "ok"
		if !a.deps.Conf.Healthy() {
			status = "degraded"
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": status, "version": buildinfo.Version})
	})
}

// bareDomain answers a request for "/", the domain opened without a path.
// Nothing is served there, and the 403 names no route, port or product
// (ARCHITECTURE.md §11.4).
func bareDomain(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSONError(w, http.StatusForbidden, "forbidden", "forbidden")
}

// gatewayMux holds the public routes (ARCHITECTURE.md §8.1, §9).
func (a *App) gatewayMux(rl *relay.Relay, mc *mcp.Server) *http.ServeMux {
	m := http.NewServeMux()
	m.Handle("GET /healthz", a.healthHandler())
	m.HandleFunc("/{$}", bareDomain)
	// MCP: per-provider paths are primary; /mcp is the merged / auto-detect endpoint.
	for p, scope := range map[string]string{"/tavily/mcp": "tavily", "/exa/mcp": "exa", "/mcp": ""} {
		h := mc.Handler(scope)
		m.Handle(p, h)
		m.Handle(p+"/", h) // the trailing slash is optional (§8.1)
	}
	// REST passthrough: SDKs set their base URL to /tavily or /exa.
	m.Handle("/tavily/", http.StripPrefix("/tavily", rl.REST("tavily")))
	m.Handle("/exa/", http.StripPrefix("/exa", rl.REST("exa")))
	m.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSONError(w, http.StatusNotFound, "not_found", "unknown path")
	}))
	return m
}

// EnsureDataDir creates the data dir (0700) and verifies it is writable, with an actionable hint.
func EnsureDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data dir %s: %w%s", dir, err, permHint(dir))
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return fmt.Errorf("data dir %s is not writable: %w%s", dir, err, permHint(dir))
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	// The directory holds plaintext upstream keys: keep others out even when it
	// was created by `mkdir` or Docker with the usual 0755 (§11.5). Failing to
	// tighten it (not the owner, or a filesystem without modes) is not fatal:
	// the files inside are 0600 either way.
	if fi, err := os.Stat(dir); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(dir, 0o700)
	}
	return nil
}

func permHint(dir string) string {
	abs, _ := filepath.Abs(dir)
	return fmt.Sprintf(" (running as uid %d; on the host run `sudo chown -R %d:%d <host dir mounted at %s>`, or set WARDEN_UID/WARDEN_GID in .env)",
		os.Getuid(), os.Getuid(), os.Getgid(), abs)
}

// Run binds listeners, serves until ctx is cancelled, then shuts down gracefully.
func (a *App) Run(ctx context.Context) error {
	type srv struct {
		name string
		s    *http.Server
		ln   net.Listener
	}
	newServer := func(name, addr string, h http.Handler) (srv, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return srv{}, fmt.Errorf("%s listen %s: %w", name, addr, err)
		}
		return srv{name: name, ln: ln, s: &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
			// No WriteTimeout: MCP long tasks stream over SSE; per-route deadlines come in P1.
			ErrorLog: slog.NewLogLogger(a.log.Handler(), slog.LevelWarn),
		}}, nil
	}

	var servers []srv
	gw, err := newServer("gateway", a.cfg.Listen, a.Gateway)
	if err != nil {
		return err
	}
	servers = append(servers, gw)
	if a.Admin != nil {
		ad, err := newServer("admin", a.cfg.AdminListen, a.Admin)
		if err != nil {
			_ = gw.ln.Close()
			return err
		}
		servers = append(servers, ad)
	}

	bgCtx, stopBG := context.WithCancel(context.Background())
	defer stopBG()
	var bg sync.WaitGroup
	bg.Add(5)
	go func() { defer bg.Done(); a.housekeeping(bgCtx) }()
	go func() { defer bg.Done(); a.deps.Out.Run(bgCtx) }()
	go func() { defer bg.Done(); a.deps.Rec.Run(bgCtx) }()
	go func() { defer bg.Done(); a.quota.Run(bgCtx) }()
	go func() { defer bg.Done(); a.keep.Run(bgCtx) }()

	errc := make(chan error, len(servers))
	for _, s := range servers {
		a.log.Info("listening", "server", s.name, "addr", s.ln.Addr().String())
		go func() {
			if err := s.s.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", s.name, err)
				return
			}
			errc <- nil
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutting down", "timeout", ShutdownTimeout.String())
	case runErr = <-errc:
		a.log.Error("server failed", "err", runErr)
	}

	sctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	for _, s := range servers {
		if err := s.s.Shutdown(sctx); err != nil {
			a.log.Warn("shutdown incomplete", "server", s.name, "err", err)
		}
	}
	// Only now: requests that were still finishing above have queued their logs.
	stopBG()
	bg.Wait()
	return runErr
}

func (a *App) flushState(ctx context.Context) {
	if err := a.deps.Pool.Flush(ctx); err != nil {
		a.log.Warn("key state flush failed", "err", err)
	}
	if err := a.deps.Out.Flush(ctx); err != nil {
		a.log.Warn("egress state flush failed", "err", err)
	}
	if err := a.deps.Limits.Flush(ctx); err != nil {
		a.log.Warn("token usage flush failed", "err", err)
	}
}

// housekeeping runs periodic maintenance until ctx is done, then flushes key
// state one last time so cooldowns and usage survive a restart.
func (a *App) housekeeping(ctx context.Context) {
	flush := time.NewTicker(5 * time.Second)
	cleanup := time.NewTicker(10 * time.Minute)
	defer flush.Stop()
	defer cleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			a.flushState(fctx)
			cancel()
			return
		case <-flush.C:
			a.flushState(ctx)
		case <-cleanup.C:
			a.auth.Cleanup(ctx)
			a.deps.Limits.Prune(ctx)
		}
	}
}
