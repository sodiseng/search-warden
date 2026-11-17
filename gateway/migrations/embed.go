// Package migrations embeds the SQL schema migrations (applied by goose at startup).
//
// Layout (ARCHITECTURE.md §2.2, §15):
//
//	sqlite/state/  → <WARDEN_DB dir>/state.db   (runtime state)
//	sqlite/logs/   → <WARDEN_DB dir>/logs.db    (request logs, stats)
//	postgres/      → one PostgreSQL database holding both
//
// Configuration is NOT stored in the database; it lives in YAML files (§14).
// Until the first public release, existing migration files may still be edited;
// after that, schema changes must be new numbered files.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed sqlite/state/*.sql sqlite/logs/*.sql postgres/*.sql
var all embed.FS

// SQLiteState returns the migrations for state.db.
func SQLiteState() fs.FS { return sub("sqlite/state") }

// SQLiteLogs returns the migrations for logs.db.
func SQLiteLogs() fs.FS { return sub("sqlite/logs") }

// Postgres returns the migrations for the single PostgreSQL database.
func Postgres() fs.FS { return sub("postgres") }

func sub(dir string) fs.FS {
	f, err := fs.Sub(all, dir)
	if err != nil {
		panic(err) // embedded paths are fixed at compile time
	}
	return f
}
