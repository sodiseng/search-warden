package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Size accounting for the request-log cap (ARCHITECTURE.md §13.1).

// LogsSize is the space the request logs take, in bytes. SQLite: pages in use
// in logs.db (free pages are returned by incremental_vacuum). PostgreSQL: the
// request_logs partitions and request_attempts, indexes included.
func (s *Store) LogsSize(ctx context.Context) (int64, error) {
	if s.isPG() {
		var n int64
		err := s.logs.r.QueryRowContext(ctx, `SELECT COALESCE(SUM(pg_total_relation_size(c.oid)), 0) FROM pg_class c
			WHERE c.relkind = 'r' AND (c.relname = 'request_attempts' OR c.relname = 'request_logs_default' OR c.relname ~ '^request_logs_p[0-9]{8}$')`).Scan(&n)
		return n, err
	}
	var pages, free, size int64
	for q, dst := range map[string]*int64{"PRAGMA page_count": &pages, "PRAGMA freelist_count": &free, "PRAGMA page_size": &size} {
		if err := s.logs.r.QueryRowContext(ctx, q).Scan(dst); err != nil {
			return 0, err
		}
	}
	return (pages - free) * size, nil
}

// LogStats describes what the request-log table holds.
type LogStats struct {
	Rows   int64
	Oldest time.Time // zero when empty
}

// RequestLogStats counts the logs and finds the oldest one.
func (s *Store) RequestLogStats(ctx context.Context) (LogStats, error) {
	var st LogStats
	var oldest sql.NullInt64
	err := s.logs.r.QueryRowContext(ctx, `SELECT COUNT(*), MIN(ts) FROM request_logs`).Scan(&st.Rows, &oldest)
	if oldest.Valid {
		st.Oldest = fromMS(oldest.Int64)
	}
	return st, err
}

// NthOldestLogTS is the time of the n-th oldest log (1-based): deleting
// everything before it plus that instant removes about n rows.
func (s *Store) NthOldestLogTS(ctx context.Context, n int64) (time.Time, error) {
	if n < 1 {
		n = 1
	}
	var ts int64
	err := s.logs.r.QueryRowContext(ctx, s.rebind(`SELECT ts FROM request_logs ORDER BY ts LIMIT 1 OFFSET ?`), n-1).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	return fromMS(ts), err
}
