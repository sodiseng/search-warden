package store

import (
	"context"
	"strings"
	"time"
)

// Audit sources.
const (
	AuditPanel  = "panel"
	AuditFile   = "file"
	AuditSystem = "system"
	AuditCLI    = "cli"
)

// AuditEntry is one append-only audit record. Diff must already be redacted.
type AuditEntry struct {
	ID     int64
	TS     time.Time
	Source string
	IP     string
	Action string
	Target string
	Diff   string
}

// AppendAudit adds an entry and returns its ID.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) (int64, error) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	var id int64
	err := s.state.w.QueryRowContext(ctx, s.rebind(
		`INSERT INTO audit_log (ts, source, ip, action, target, diff) VALUES (?,?,?,?,?,?) RETURNING id`),
		ms(e.TS), e.Source, cleanText(e.IP), e.Action, cleanText(e.Target), cleanText(e.Diff)).Scan(&id)
	return id, err
}

// AuditQuery filters ListAudit. Zero values mean "any". Results are newest first;
// pass the last ID seen as BeforeID to page.
type AuditQuery struct {
	Source   string
	Action   string // prefix match, e.g. "key." matches "key.update"
	BeforeID int64
	Limit    int // default 100, max 1000
}

// ListAudit returns entries matching q.
func (s *Store) ListAudit(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	var (
		where []string
		args  []any
	)
	if q.Source != "" {
		where, args = append(where, "source = ?"), append(args, q.Source)
	}
	if q.Action != "" {
		where, args = append(where, `action LIKE ? ESCAPE '\'`), append(args, escapeLike(q.Action)+"%")
	}
	if q.BeforeID > 0 {
		where, args = append(where, "id < ?"), append(args, q.BeforeID)
	}
	query := `SELECT id, ts, source, ip, action, target, diff FROM audit_log`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, clampLimit(q.Limit, 100, 1000))

	rows, err := s.state.r.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var (
			e  AuditEntry
			ts int64
		)
		if err := rows.Scan(&e.ID, &ts, &e.Source, &e.IP, &e.Action, &e.Target, &e.Diff); err != nil {
			return nil, err
		}
		e.TS = fromMS(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// escapeLike escapes LIKE wildcards so the filter is a literal prefix
// (used with ESCAPE '\', supported by SQLite and PostgreSQL).
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
}

func clampLimit(n, def, max int) int {
	switch {
	case n <= 0:
		return def
	case n > max:
		return max
	default:
		return n
	}
}
