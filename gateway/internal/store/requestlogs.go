package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// RequestLog is one row of the request monitor (ARCHITECTURE.md §13.2).
type RequestLog struct {
	ID                int64
	TS                time.Time
	RequestID         string
	ClientIP          string
	IPSource          string
	UserAgent         string
	TokenID           string
	Provider          string
	Surface           string // mcp | rest
	Operation         string
	KeyID             string
	EgressID          string
	ExitIP            string
	Attempts          int
	Status            int
	Outcome           string
	ErrorKind         string
	LatencyMS         int
	UpstreamLatencyMS int
	CostCredits       int64
	CostMicroUSD      int64
	CostEstimated     bool
	ReqBytes          int64
	RespBytes         int64
	QueryPreview      string
}

// Attempt is one upstream try, stored only for requests with Attempts > 1.
type Attempt struct {
	RequestID string
	N         int
	TS        time.Time
	KeyID     string
	EgressID  string
	Status    int
	ErrorKind string
	LatencyMS int
}

const requestLogCols = `ts, request_id, client_ip, ip_source, user_agent, token_id, provider, surface, operation,
	key_id, egress_id, exit_ip, attempts, status, outcome, error_kind, latency_ms, upstream_latency_ms,
	cost_credits, cost_micro_usd, cost_estimated, req_bytes, resp_bytes, query_preview`

const requestLogNCols = 24

// MaxUserAgentLen bounds the stored User-Agent (§13.1: keep the DB small).
const MaxUserAgentLen = 64

// InsertRequestLogs writes a batch of logs and their attempts in one transaction.
// Called by the async log writer (§13.1), never on the request path.
func (s *Store) InsertRequestLogs(ctx context.Context, logs []RequestLog, attempts []Attempt) error {
	if len(logs) == 0 && len(attempts) == 0 {
		return nil
	}
	tx, err := s.logs.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if len(logs) > 0 {
		stmt, err := tx.PrepareContext(ctx, s.rebind(`INSERT INTO request_logs (`+requestLogCols+`) VALUES `+placeholders(requestLogNCols)))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, l := range logs {
			if _, err := stmt.ExecContext(ctx, ms(l.TS), l.RequestID, l.ClientIP, l.IPSource, truncateRunes(cleanText(l.UserAgent), MaxUserAgentLen),
				l.TokenID, l.Provider, l.Surface, l.Operation, l.KeyID, l.EgressID, l.ExitIP, l.Attempts, l.Status,
				l.Outcome, l.ErrorKind, l.LatencyMS, l.UpstreamLatencyMS, l.CostCredits, l.CostMicroUSD,
				l.CostEstimated, l.ReqBytes, l.RespBytes, cleanText(l.QueryPreview)); err != nil {
				return err
			}
		}
	}
	if len(attempts) > 0 {
		stmt, err := tx.PrepareContext(ctx, s.rebind(`INSERT INTO request_attempts
			(request_id, n, ts, key_id, egress_id, status, error_kind, latency_ms) VALUES `+placeholders(8)+`
			ON CONFLICT (request_id, n) DO NOTHING`))
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, a := range attempts {
			if _, err := stmt.ExecContext(ctx, a.RequestID, a.N, ms(a.TS), a.KeyID, a.EgressID, a.Status, a.ErrorKind, a.LatencyMS); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// LogQuery filters QueryRequestLogs. Results are newest first; page with BeforeID.
type LogQuery struct {
	From, To  time.Time // To exclusive; zero = unbounded
	Provider  string
	TokenID   string
	KeyID     string
	ClientIP  string
	Operation string
	Status    int // exact status; 0 = any
	ErrorOnly bool
	BeforeID  int64
	Limit     int // default 100, max 1000
}

// QueryRequestLogs returns logs matching q.
func (s *Store) QueryRequestLogs(ctx context.Context, q LogQuery) ([]RequestLog, error) {
	var (
		where []string
		args  []any
	)
	add := func(cond string, v any) { where, args = append(where, cond), append(args, v) }
	if !q.From.IsZero() {
		add("ts >= ?", ms(q.From))
	}
	if !q.To.IsZero() {
		add("ts < ?", ms(q.To))
	}
	if q.Provider != "" {
		add("provider = ?", q.Provider)
	}
	if q.TokenID != "" {
		add("token_id = ?", q.TokenID)
	}
	if q.KeyID != "" {
		add("key_id = ?", q.KeyID)
	}
	if q.ClientIP != "" {
		add("client_ip = ?", q.ClientIP)
	}
	if q.Operation != "" {
		add("operation = ?", q.Operation)
	}
	if q.Status != 0 {
		add("status = ?", q.Status)
	}
	if q.ErrorOnly {
		where = append(where, "(status >= 400 OR error_kind <> '')")
	}
	if q.BeforeID > 0 {
		add("id < ?", q.BeforeID)
	}
	query := `SELECT id, ` + requestLogCols + ` FROM request_logs`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, clampLimit(q.Limit, 100, 1000))

	rows, err := s.logs.r.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestLog
	for rows.Next() {
		var (
			l  RequestLog
			ts int64
		)
		if err := rows.Scan(&l.ID, &ts, &l.RequestID, &l.ClientIP, &l.IPSource, &l.UserAgent, &l.TokenID, &l.Provider,
			&l.Surface, &l.Operation, &l.KeyID, &l.EgressID, &l.ExitIP, &l.Attempts, &l.Status, &l.Outcome,
			&l.ErrorKind, &l.LatencyMS, &l.UpstreamLatencyMS, &l.CostCredits, &l.CostMicroUSD, &l.CostEstimated,
			&l.ReqBytes, &l.RespBytes, &l.QueryPreview); err != nil {
			return nil, err
		}
		l.TS = fromMS(ts)
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListAttempts returns the attempts of one request in order.
func (s *Store) ListAttempts(ctx context.Context, requestID string) ([]Attempt, error) {
	rows, err := s.logs.r.QueryContext(ctx, s.rebind(`SELECT request_id, n, ts, key_id, egress_id, status, error_kind, latency_ms
		FROM request_attempts WHERE request_id = ? ORDER BY n`), requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var (
			a  Attempt
			ts int64
		)
		if err := rows.Scan(&a.RequestID, &a.N, &ts, &a.KeyID, &a.EgressID, &a.Status, &a.ErrorKind, &a.LatencyMS); err != nil {
			return nil, err
		}
		a.TS = fromMS(ts)
		out = append(out, a)
	}
	return out, rows.Err()
}

// purgeBatch bounds each DELETE so the SQLite writer lock is held only briefly.
const purgeBatch = 5000

// PurgeRequestLogsBefore deletes logs and attempts older than cutoff (rolling
// daily retention, §13.1). SQLite deletes in batches then runs incremental_vacuum;
// PostgreSQL drops whole expired daily partitions and deletes stragglers from
// the default/boundary partitions.
func (s *Store) PurgeRequestLogsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	c := ms(cutoff)
	var total int64
	if s.isPG() {
		n, err := s.dropExpiredPartitions(ctx, c)
		if err != nil {
			return 0, err
		}
		total += n
	}
	for _, del := range []string{
		`DELETE FROM request_logs WHERE id IN (SELECT id FROM request_logs WHERE ts < ? LIMIT ?)`,
		`DELETE FROM request_attempts WHERE request_id IN (SELECT request_id FROM request_attempts WHERE ts < ? LIMIT ?)`,
	} {
		for {
			res, err := s.logs.w.ExecContext(ctx, s.rebind(del), c, purgeBatch)
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += n
			if n < purgeBatch || ctx.Err() != nil {
				break
			}
		}
	}
	if !s.isPG() {
		if _, err := s.logs.w.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return total, fmt.Errorf("incremental_vacuum: %w", err)
		}
	}
	return total, nil
}

// truncateRunes cuts s to at most n characters without splitting UTF-8 sequences.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
