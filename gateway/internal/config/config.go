// Package config loads the process-level startup parameters (ARCHITECTURE.md §14.4).
//
// Only bootstrap settings live here: listen addresses, data dir, database and
// reverse-proxy hints. Everything else (keys, egresses, routing, logs, ...) is
// file-backed runtime configuration owned by package confstore.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// AdminListenShared is the WARDEN_ADMIN_LISTEN value that mounts the admin panel
// on the gateway listener under AdminBasePath instead of a separate port. It is the
// default: one port serves both the gateway and the panel (/admin/ by default).
const AdminListenShared = "shared"

// Database drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// Gateway path prefixes that a shared-mode admin base path must not shadow.
var reservedGatewayPrefixes = []string{"/mcp", "/tavily", "/exa", "/healthz", "/metrics"}

// DB describes the selected storage backend.
type DB struct {
	Driver    string // DriverSQLite or DriverPostgres
	SQLiteDir string // directory holding state.db and logs.db (sqlite only)
	DSN       string // connection string (postgres only); contains credentials
}

// Redacted returns a log-safe description of the database target.
func (d DB) Redacted() string {
	if d.Driver == DriverSQLite {
		return "sqlite://" + d.SQLiteDir
	}
	u, err := url.Parse(d.DSN)
	if err != nil {
		return "postgres://<unparseable>"
	}
	return u.Redacted()
}

// Config is the immutable startup configuration.
type Config struct {
	Listen         string // gateway listener, e.g. ":18683"
	AdminListen    string // admin listener, e.g. ":18684"; empty when AdminShared
	AdminShared    bool   // admin mounted on the gateway listener
	AdminBasePath  string // always starts and ends with "/"
	AdminPublicURL string // external admin URL behind a reverse proxy (Origin/CSRF checks)
	PublicURL      string // external gateway URL (shown in the panel's client-config snippets)
	CookieSecure   bool
	DataDir        string
	DB             DB
	LogLevel       slog.Level
	LogFormat      string // "json" or "text"

	// AdminPassword is only consumed on first start when config.yaml has no admin password.
	AdminPassword string
	// MetricsToken enables /metrics (Bearer); empty = disabled.
	MetricsToken string

	// Warnings are non-fatal configuration smells, logged at startup.
	Warnings []string
}

// LogValue keeps secrets out of structured logs.
func (c Config) LogValue() slog.Value {
	admin := c.AdminListen
	if c.AdminShared {
		admin = "shared(" + c.Listen + c.AdminBasePath + ")"
	}
	return slog.GroupValue(
		slog.String("listen", c.Listen),
		slog.String("admin", admin),
		slog.String("admin_base_path", c.AdminBasePath),
		slog.String("data_dir", c.DataDir),
		slog.String("db", c.DB.Redacted()),
		slog.Bool("cookie_secure", c.CookieSecure),
		slog.Bool("admin_password_env_set", c.AdminPassword != ""),
		slog.Bool("metrics_enabled", c.MetricsToken != ""),
	)
}

// Load reads configuration through getenv (os.Getenv in production; a map in tests).
func Load(getenv func(string) string) (Config, error) {
	env := func(k, def string) string {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
		return def
	}
	var errs []error
	c := Config{
		Listen:         env("WARDEN_LISTEN", ":18683"),
		DataDir:        env("WARDEN_DATA_DIR", "./data"),
		PublicURL:      env("WARDEN_PUBLIC_URL", ""),
		AdminPublicURL: env("WARDEN_ADMIN_PUBLIC_URL", ""),
		LogFormat:      strings.ToLower(env("WARDEN_LOG_FORMAT", "json")),
		AdminPassword:  getenv("WARDEN_ADMIN_PASSWORD"), // not trimmed: whitespace may be intentional
		MetricsToken:   env("WARDEN_METRICS_TOKEN", ""),
	}

	if err := validateListen(c.Listen); err != nil {
		errs = append(errs, fmt.Errorf("WARDEN_LISTEN: %w", err))
	}

	if t := c.MetricsToken; t != "" && (len(t) < 16 || strings.ContainsAny(t, " \t\r\n")) {
		errs = append(errs, errors.New("WARDEN_METRICS_TOKEN must be at least 16 characters without spaces"))
	}
	// Default: the panel rides on the gateway listener under /admin/, so only one port
	// has to be exposed. Set WARDEN_ADMIN_LISTEN=:18684 for a separate port again.
	adminListen := env("WARDEN_ADMIN_LISTEN", AdminListenShared)
	if strings.EqualFold(adminListen, AdminListenShared) {
		c.AdminShared = true
	} else {
		c.AdminListen = adminListen
		if err := validateListen(adminListen); err != nil {
			errs = append(errs, fmt.Errorf("WARDEN_ADMIN_LISTEN: %w", err))
		} else if sameListen(adminListen, c.Listen) {
			errs = append(errs, errors.New("WARDEN_ADMIN_LISTEN equals WARDEN_LISTEN; use WARDEN_ADMIN_LISTEN=shared (the default) to serve both on one port"))
		}
	}

	defBase := "/"
	if c.AdminShared {
		defBase = "/admin/"
	}
	base, err := normalizeBasePath(env("WARDEN_ADMIN_BASE_PATH", defBase))
	if err != nil {
		errs = append(errs, fmt.Errorf("WARDEN_ADMIN_BASE_PATH: %w", err))
	} else if c.AdminShared {
		if base == "/" {
			errs = append(errs, errors.New("WARDEN_ADMIN_BASE_PATH must not be \"/\" when WARDEN_ADMIN_LISTEN=shared"))
		}
		for _, p := range reservedGatewayPrefixes {
			// Collides if the admin base is the reserved prefix, lives under it, or contains it.
			if strings.HasPrefix(base, p+"/") || strings.HasPrefix(p+"/", base) {
				errs = append(errs, fmt.Errorf("WARDEN_ADMIN_BASE_PATH %q collides with gateway path %q", base, p))
			}
		}
	}
	c.AdminBasePath = base

	for _, u := range []struct{ name, val string }{
		{"WARDEN_PUBLIC_URL", c.PublicURL}, {"WARDEN_ADMIN_PUBLIC_URL", c.AdminPublicURL},
	} {
		if u.val == "" {
			continue
		}
		if err := validateHTTPURL(u.val); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", u.name, err))
		}
	}

	if v := env("WARDEN_COOKIE_SECURE", "false"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("WARDEN_COOKIE_SECURE: %q is not a boolean", v))
		}
		c.CookieSecure = b
	}
	if strings.HasPrefix(strings.ToLower(c.AdminPublicURL), "https://") && !c.CookieSecure {
		c.Warnings = append(c.Warnings, "WARDEN_ADMIN_PUBLIC_URL is https but WARDEN_COOKIE_SECURE=false; set it to true when TLS terminates at the reverse proxy")
	}

	switch lvl := strings.ToLower(env("WARDEN_LOG_LEVEL", "info")); lvl {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn", "warning":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		errs = append(errs, fmt.Errorf("WARDEN_LOG_LEVEL: unknown level %q (debug|info|warn|error)", lvl))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("WARDEN_LOG_FORMAT: %q must be json or text", c.LogFormat))
	}

	db, err := parseDB(env("WARDEN_DB", ""), c.DataDir)
	if err != nil {
		errs = append(errs, fmt.Errorf("WARDEN_DB: %w", err))
	}
	c.DB = db

	return c, errors.Join(errs...)
}

func validateListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port (e.g. \":18683\" or \"127.0.0.1:18683\")", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%q has an invalid port", addr)
	}
	return nil
}

// sameListen reports whether two listen addresses would bind the same port on overlapping hosts.
func sameListen(a, b string) bool {
	ha, pa, _ := net.SplitHostPort(a)
	hb, pb, _ := net.SplitHostPort(b)
	if pa != pb || pa == "0" {
		return false
	}
	wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	return ha == hb || wild(ha) || wild(hb)
}

func normalizeBasePath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%q must start with /", p)
	}
	if strings.ContainsAny(p, "?#*{} \t") || strings.Contains(p, "//") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
		return "", fmt.Errorf("%q contains invalid characters", p)
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p, nil
}

func validateHTTPURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q must be an absolute http(s) URL", s)
	}
	return nil
}

func parseDB(raw, dataDir string) (DB, error) {
	if raw == "" {
		return DB{Driver: DriverSQLite, SQLiteDir: filepath.Join(dataDir, "db")}, nil
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "sqlite://"):
		dir := raw[len("sqlite://"):]
		if dir == "" {
			return DB{}, errors.New("sqlite:// needs a directory, e.g. sqlite:///data/db")
		}
		return DB{Driver: DriverSQLite, SQLiteDir: filepath.Clean(dir)}, nil
	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return DB{}, errors.New("invalid postgres URL; expected postgres://user:pass@host:5432/dbname")
		}
		return DB{Driver: DriverPostgres, DSN: raw}, nil
	default:
		return DB{}, errors.New("must start with sqlite:// or postgres://")
	}
}
