// Package store persists runtime state and request logs in SQLite (default) or
// PostgreSQL (ARCHITECTURE.md §2.2, §13, §15). Configuration is not stored here.
//
// Queries are written once with "?" placeholders and rebound to "$n" for
// PostgreSQL. Timestamps are Unix milliseconds in both backends.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers "sqlite" (pure Go, CGO_ENABLED=0)

	"github.com/sodiseng/search-warden/gateway/internal/config"
	"github.com/sodiseng/search-warden/gateway/migrations"
)

// ErrNotFound is returned by single-row lookups.
var ErrNotFound = errors.New("store: not found")

// pool is a writer/reader pair. For SQLite the writer is limited to one
// connection (SQLite allows one writer; serializing in-process avoids
// SQLITE_BUSY churn) and readers are query_only. For PostgreSQL both are the same pool.
type pool struct {
	w, r *sql.DB
}

func (p pool) close() error {
	err := p.w.Close()
	if p.r != p.w {
		err = errors.Join(err, p.r.Close())
	}
	return err
}

// Store is safe for concurrent use.
type Store struct {
	driver string
	state  pool
	logs   pool
}

// Driver returns config.DriverSQLite or config.DriverPostgres.
func (s *Store) Driver() string { return s.driver }

func (s *Store) isPG() bool { return s.driver == config.DriverPostgres }

// Open connects, applies pending migrations and returns a ready Store.
func Open(ctx context.Context, cfg config.DB) (*Store, error) {
	switch cfg.Driver {
	case config.DriverSQLite:
		return openSQLite(ctx, cfg.SQLiteDir)
	case config.DriverPostgres:
		return openPostgres(ctx, cfg.DSN)
	default:
		return nil, fmt.Errorf("store: unknown driver %q", cfg.Driver)
	}
}

func openSQLite(ctx context.Context, dir string) (_ *Store, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", dir, err)
	}
	s := &Store{driver: config.DriverSQLite}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	if s.state, err = openSQLiteFile(filepath.Join(dir, "state.db"), false); err != nil {
		return nil, err
	}
	if s.logs, err = openSQLiteFile(filepath.Join(dir, "logs.db"), true); err != nil {
		return nil, err
	}
	if err := migrate(ctx, goose.DialectSQLite3, s.state.w, migrations.SQLiteState()); err != nil {
		return nil, fmt.Errorf("store: migrate state.db: %w", err)
	}
	if err := migrate(ctx, goose.DialectSQLite3, s.logs.w, migrations.SQLiteLogs()); err != nil {
		return nil, fmt.Errorf("store: migrate logs.db: %w", err)
	}
	return s, nil
}

func sqliteDSN(path string, readOnly, incrementalVacuum bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		if incrementalVacuum {
			q.Add("_pragma", "auto_vacuum(INCREMENTAL)") // only effective before the first table exists
		}
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Set("_txlock", "immediate") // take the write lock at BEGIN: no deadlocking lock upgrades
	}
	return "file:" + filepath.ToSlash(path) + "?" + q.Encode()
}

func openSQLiteFile(path string, incrementalVacuum bool) (pool, error) {
	// Pre-create with 0600 so the key-bearing data stays private (SQLite reuses
	// the main file's mode for -wal/-shm).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return pool{}, fmt.Errorf("store: open %s: %w", path, err)
	}
	_ = f.Close()

	w, err := sql.Open("sqlite", sqliteDSN(path, false, incrementalVacuum))
	if err != nil {
		return pool{}, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxIdleTime(0)
	if err := w.Ping(); err != nil {
		_ = w.Close()
		return pool{}, fmt.Errorf("store: open %s: %w", path, err)
	}
	r, err := sql.Open("sqlite", sqliteDSN(path, true, false))
	if err != nil {
		_ = w.Close()
		return pool{}, err
	}
	r.SetMaxOpenConns(4)
	return pool{w: w, r: r}, nil
}

func openPostgres(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: postgres: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)

	// The DB container may still be starting (compose); retry briefly.
	var pingErr error
	for i := 0; i < 30; i++ {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		pingErr = db.PingContext(pctx)
		cancel()
		if pingErr == nil || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if pingErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: postgres not reachable: %w", pingErr)
	}
	p := pool{w: db, r: db}
	s := &Store{driver: config.DriverPostgres, state: p, logs: p}
	if err := migrate(ctx, goose.DialectPostgres, db, migrations.Postgres()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate postgres: %w", err)
	}
	if err := s.EnsureLogPartitions(ctx, time.Now()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func migrate(ctx context.Context, dialect goose.Dialect, db *sql.DB, fsys fs.FS) error {
	p, err := goose.NewProvider(dialect, db, fsys, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
}

// Close releases all connections.
func (s *Store) Close() error {
	var err error
	if s.state.w != nil {
		err = s.state.close()
	}
	if s.logs.w != nil && s.logs.w != s.state.w {
		err = errors.Join(err, s.logs.close())
	}
	return err
}

// Ping checks both databases (used by /healthz in P1).
func (s *Store) Ping(ctx context.Context) error {
	if err := s.state.r.PingContext(ctx); err != nil {
		return err
	}
	return s.logs.r.PingContext(ctx)
}

// rebind converts "?" placeholders to "$1..$n" for PostgreSQL.
// Queries in this package never contain "?" inside string literals.
func (s *Store) rebind(q string) string {
	if !s.isPG() {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 16)
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// ms converts a time to Unix milliseconds; the zero time maps to 0.
func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// fromMS is the inverse of ms.
func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// cleanText makes client-controlled text storable: PostgreSQL rejects invalid
// UTF-8 and NUL bytes, and one bad value would fail its whole insert batch.
func cleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, string(utf8.RuneError)), string(rune(0)), "")
}

// placeholders returns "(?,?,?)" with n marks.
func placeholders(n int) string {
	return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}
