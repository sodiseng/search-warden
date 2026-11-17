// Command warden is the search-warden gateway.
//
//	warden [serve]      run the gateway (default)
//	warden healthcheck  probe the local /healthz (used by the Docker HEALTHCHECK; no curl in distroless)
//	warden version      print version
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sodiseng/search-warden/gateway/internal/app"
	"github.com/sodiseng/search-warden/gateway/internal/buildinfo"
	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/internal/confstore"
	"github.com/sodiseng/search-warden/gateway/internal/limits"
	"github.com/sodiseng/search-warden/gateway/internal/obs"
	"github.com/sodiseng/search-warden/gateway/internal/outbound"
	"github.com/sodiseng/search-warden/gateway/internal/pool"
	"github.com/sodiseng/search-warden/gateway/internal/store"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `Usage: warden [command]

Commands:
  serve        Run the gateway (default)
  healthcheck  Exit 0 if the local gateway answers /healthz with 200
  version      Print version information

Configuration is read from WARDEN_* environment variables; see README.md.
`

func run(args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serve(stderr)
	case "healthcheck":
		return healthcheck(stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "warden", buildinfo.String())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

func newLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func serve(stderr io.Writer) int {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	log := newLogger(cfg, stderr)
	log.Info("starting warden", "version", buildinfo.Version, "commit", buildinfo.Commit, "config", cfg)
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}
	if err := app.EnsureDataDir(cfg.DataDir); err != nil {
		log.Error("data dir check failed", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conf, err := confstore.Open(cfg.DataDir, confstore.Options{AdminPasswordEnv: cfg.AdminPassword, Logger: log})
	if err != nil {
		var errs confstore.Errors
		if errors.As(err, &errs) {
			for _, e := range errs {
				log.Error("config error", "file", e.File, "line", e.Line, "error", e.Msg)
			}
			log.Error("fix the files above in the data directory and start again", "data_dir", cfg.DataDir)
		} else {
			log.Error("config init failed", "err", err)
		}
		return 1
	}
	snap := conf.Snapshot()
	log.Info("config loaded", "keys", len(snap.Keys), "egresses", len(snap.Egresses.Nodes()), "tokens", len(snap.Tokens))
	go conf.Watch(ctx)

	st, err := store.Open(ctx, cfg.DB)
	if err != nil {
		log.Error("database init failed", "db", cfg.DB.Redacted(), "err", err)
		return 1
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Warn("database close", "err", err)
		}
	}()
	log.Info("database ready", "driver", st.Driver(), "db", cfg.DB.Redacted())

	out := outbound.New(outbound.Options{Store: st, Logger: log})
	keys := pool.New(pool.Options{Store: st, EgressUsable: out.Usable, Logger: log})
	lim := limits.New(st, log, nil)
	if err := errors.Join(out.Load(ctx), keys.Load(ctx), lim.Load(ctx)); err != nil {
		log.Error("loading runtime state failed", "err", err)
		return 1
	}
	out.Apply(conf.Snapshot())
	keys.Apply(conf.Snapshot())

	a, err := app.New(cfg, log, app.Deps{Conf: conf, Store: st, Pool: keys, Out: out, Limits: lim, Rec: obs.NewWriter(st, log)})
	if err != nil {
		log.Error("init failed", "err", err)
		return 1
	}
	if err := a.Run(ctx); err != nil {
		log.Error("exited with error", "err", err)
		return 1
	}
	log.Info("bye")
	return 0
}

func healthcheck(stderr io.Writer) int {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "invalid configuration: %v\n", err)
		return 1
	}
	host, port, _ := net.SplitHostPort(cfg.Listen)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintln(stderr, "unhealthy:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(stderr, "unhealthy: status", resp.StatusCode)
		return 1
	}
	return 0
}
