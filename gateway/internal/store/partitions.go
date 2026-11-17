package store

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Daily partitions of request_logs (PostgreSQL only), named request_logs_pYYYYMMDD
// and covering [UTC midnight, next UTC midnight) in Unix ms.

const partitionsAhead = 3

var partitionNameRe = regexp.MustCompile(`^request_logs_p(\d{8})$`)

func dayStartUTC(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// EnsureLogPartitions creates partitions for today and the next few days. Past
// days are not back-filled: rows from downtime gaps live in the default partition
// (creating a partition that overlaps existing default rows would fail).
// No-op for SQLite. Call it at startup and daily.
func (s *Store) EnsureLogPartitions(ctx context.Context, now time.Time) error {
	if !s.isPG() {
		return nil
	}
	start := dayStartUTC(now)
	for i := 0; i <= partitionsAhead; i++ {
		from := start.AddDate(0, 0, i)
		to := from.AddDate(0, 0, 1)
		name := "request_logs_p" + from.Format("20060102")
		var exists bool
		if err := s.logs.r.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
			return fmt.Errorf("check partition %s: %w", name, err)
		}
		if exists {
			continue
		}
		// Identifiers and bounds are generated here, never from user input.
		ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF request_logs FOR VALUES FROM (%d) TO (%d)`,
			name, from.UnixMilli(), to.UnixMilli())
		if _, err := s.logs.w.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("create partition %s: %w", name, err)
		}
	}
	return nil
}

// dropExpiredPartitions drops daily partitions whose whole range is before cutoffMS.
// It returns the number of rows removed (counted before dropping).
func (s *Store) dropExpiredPartitions(ctx context.Context, cutoffMS int64) (int64, error) {
	rows, err := s.logs.r.QueryContext(ctx, `SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'request_logs'::regclass`)
	if err != nil {
		return 0, err
	}
	var expired []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return 0, err
		}
		m := partitionNameRe.FindStringSubmatch(name)
		if m == nil {
			continue // default partition or foreign object
		}
		day, err := time.ParseInLocation("20060102", m[1], time.UTC)
		if err != nil {
			continue
		}
		if day.AddDate(0, 0, 1).UnixMilli() <= cutoffMS {
			expired = append(expired, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var total int64
	for _, name := range expired {
		var n int64
		if err := s.logs.r.QueryRowContext(ctx, `SELECT count(*) FROM `+strconv.Quote(name)).Scan(&n); err != nil {
			return total, err
		}
		if _, err := s.logs.w.ExecContext(ctx, `DROP TABLE IF EXISTS `+strconv.Quote(name)); err != nil {
			return total, fmt.Errorf("drop partition %s: %w", name, err)
		}
		total += n
	}
	return total, nil
}
