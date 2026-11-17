package config

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	c, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Listen != ":18683" || c.AdminListen != "" || !c.AdminShared {
		t.Errorf("listen defaults wrong: %+v", c)
	}
	if c.AdminBasePath != "/admin/" {
		t.Errorf("AdminBasePath = %q, want /admin/", c.AdminBasePath)
	}
	if c.DB.Driver != DriverSQLite || c.DB.SQLiteDir != filepath.Join("./data", "db") {
		t.Errorf("DB default = %+v", c.DB)
	}
	if c.LogLevel != slog.LevelInfo || c.LogFormat != "json" || c.CookieSecure {
		t.Errorf("misc defaults wrong: %+v", c)
	}
}

func TestSharedAdmin(t *testing.T) {
	c, err := Load(envMap(map[string]string{"WARDEN_ADMIN_LISTEN": "shared"}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AdminShared || c.AdminListen != "" || c.AdminBasePath != "/admin/" {
		t.Errorf("shared mode wrong: %+v", c)
	}

	for _, bad := range []string{"/", "/mcp", "/tavily/sub", "/exa/", "/healthz"} {
		_, err := Load(envMap(map[string]string{"WARDEN_ADMIN_LISTEN": "shared", "WARDEN_ADMIN_BASE_PATH": bad}))
		if err == nil {
			t.Errorf("base path %q in shared mode should be rejected", bad)
		}
	}
	c, err = Load(envMap(map[string]string{"WARDEN_ADMIN_LISTEN": "shared", "WARDEN_ADMIN_BASE_PATH": "/panel"}))
	if err != nil || c.AdminBasePath != "/panel/" {
		t.Errorf("custom base path: %q, %v", c.AdminBasePath, err)
	}
}

func TestListenConflict(t *testing.T) {
	_, err := Load(envMap(map[string]string{"WARDEN_LISTEN": ":9000", "WARDEN_ADMIN_LISTEN": "127.0.0.1:9000"}))
	if err == nil || !strings.Contains(err.Error(), "shared") {
		t.Errorf("expected same-port error, got %v", err)
	}
}

func TestDBParsing(t *testing.T) {
	c, err := Load(envMap(map[string]string{"WARDEN_DB": "sqlite:///data/db"}))
	if err != nil || c.DB.Driver != DriverSQLite || c.DB.SQLiteDir != filepath.Clean("/data/db") {
		t.Errorf("sqlite: %+v %v", c.DB, err)
	}

	dsn := "postgres://warden:s3cret@pg:5432/warden?sslmode=disable"
	c, err = Load(envMap(map[string]string{"WARDEN_DB": dsn}))
	if err != nil || c.DB.Driver != DriverPostgres || c.DB.DSN != dsn {
		t.Fatalf("postgres: %+v %v", c.DB, err)
	}
	if strings.Contains(c.DB.Redacted(), "s3cret") {
		t.Errorf("Redacted leaks password: %s", c.DB.Redacted())
	}

	for _, bad := range []string{"mysql://x", "sqlite://", "postgres://"} {
		if _, err := Load(envMap(map[string]string{"WARDEN_DB": bad})); err == nil {
			t.Errorf("WARDEN_DB=%q should fail", bad)
		}
	}
}

func TestValidationErrorsAreJoined(t *testing.T) {
	_, err := Load(envMap(map[string]string{
		"WARDEN_LISTEN":        "nonsense",
		"WARDEN_LOG_LEVEL":     "loud",
		"WARDEN_COOKIE_SECURE": "maybe",
		"WARDEN_PUBLIC_URL":    "ftp://x",
	}))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"WARDEN_LISTEN", "WARDEN_LOG_LEVEL", "WARDEN_COOKIE_SECURE", "WARDEN_PUBLIC_URL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %s: %v", want, err)
		}
	}
}

func TestCookieSecureWarning(t *testing.T) {
	c, err := Load(envMap(map[string]string{"WARDEN_ADMIN_PUBLIC_URL": "https://admin.example.com"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) == 0 {
		t.Error("expected warning about WARDEN_COOKIE_SECURE")
	}
}

func TestLogValueHidesSecrets(t *testing.T) {
	c, err := Load(envMap(map[string]string{
		"WARDEN_ADMIN_PASSWORD": "hunter2-very-secret",
		"WARDEN_DB":             "postgres://u:pgpass@h/db",
	}))
	if err != nil {
		t.Fatal(err)
	}
	s := c.LogValue().String()
	for _, secret := range []string{"hunter2-very-secret", "pgpass"} {
		if strings.Contains(s, secret) {
			t.Errorf("LogValue leaks %q: %s", secret, s)
		}
	}
}
