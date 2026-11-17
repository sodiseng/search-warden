package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Dashboard and request-monitor queries over request_logs (ARCHITECTURE.md
// §13.3). They run on the raw logs: for a small team's 30 days that is cheap,
// and there is no rollup to drift out of sync.

// errCond marks a failed request: an error status, or a gateway-side error kind.
const errCond = `(status >= 400 OR error_kind <> '')`

// Totals summarizes requests in [From, To).
type Totals struct {
	Requests, Errors int64
	Credits          int64 // Tavily
	MicroUSD         int64 // Exa
	P50, P95         int   // total latency, ms (0 when there are no requests)
}

// Totals returns the summary of one time range.
func (s *Store) Totals(ctx context.Context, from, to time.Time) (Totals, error) {
	var t Totals
	err := s.logs.r.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN `+errCond+` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(cost_credits), 0), COALESCE(SUM(cost_micro_usd), 0) FROM request_logs WHERE ts >= ? AND ts < ?`),
		ms(from), ms(to)).Scan(&t.Requests, &t.Errors, &t.Credits, &t.MicroUSD)
	if err != nil || t.Requests == 0 {
		return t, err
	}
	pct := func(p float64) (int, error) {
		var v int
		err := s.logs.r.QueryRowContext(ctx, s.rebind(`SELECT latency_ms FROM request_logs WHERE ts >= ? AND ts < ?
			ORDER BY latency_ms LIMIT 1 OFFSET ?`), ms(from), ms(to), int64(p*float64(t.Requests-1))).Scan(&v)
		return v, err
	}
	if t.P50, err = pct(0.50); err != nil {
		return t, err
	}
	t.P95, err = pct(0.95)
	return t, err
}

// SeriesPoint is one time bucket of one provider.
type SeriesPoint struct {
	Bucket           time.Time
	Provider         string
	Requests, Errors int64
}

// Series counts requests per bucket and provider in [from, to), oldest first.
// Buckets are aligned to multiples of bucket since the Unix epoch.
func (s *Store) Series(ctx context.Context, from, to time.Time, bucket time.Duration) ([]SeriesPoint, error) {
	b := bucket.Milliseconds()
	if b <= 0 {
		return nil, errors.New("store: bucket must be positive")
	}
	rows, err := s.logs.r.QueryContext(ctx, s.rebind(`SELECT (ts / ?) * ? AS b, provider, COUNT(*),
		SUM(CASE WHEN `+errCond+` THEN 1 ELSE 0 END) FROM request_logs WHERE ts >= ? AND ts < ?
		GROUP BY b, provider ORDER BY b, provider`), b, b, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var (
			p  SeriesPoint
			ts int64
		)
		if err := rows.Scan(&ts, &p.Provider, &p.Requests, &p.Errors); err != nil {
			return nil, err
		}
		p.Bucket = time.UnixMilli(ts)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Dimensions for Top. Values are SQL expressions over request_logs columns,
// never user input.
const (
	ByTool      = "tool"
	ByToken     = "token"
	ByKey       = "key"
	ByClientIP  = "client_ip"
	ByErrorKind = "error_kind"
)

var topExpr = map[string]string{
	ByTool:      `provider || '/' || operation`,
	ByToken:     `token_id`,
	ByKey:       `key_id`,
	ByClientIP:  `client_ip`,
	ByErrorKind: `CASE WHEN error_kind <> '' THEN error_kind ELSE outcome END`,
}

// TopRow is one value of a dimension and its traffic.
type TopRow struct {
	Value            string
	Requests, Errors int64
	Credits          int64
	MicroUSD         int64
	LastTS           time.Time
}

// Top lists the busiest values of a dimension in [from, to), most requests
// first. ByErrorKind counts failed requests only. n <= 0 means no limit.
func (s *Store) Top(ctx context.Context, from, to time.Time, dim string, n int) ([]TopRow, error) {
	expr, ok := topExpr[dim]
	if !ok {
		return nil, fmt.Errorf("store: unknown dimension %q", dim)
	}
	where := `ts >= ? AND ts < ? AND ` + expr + ` <> ''`
	if dim == ByErrorKind {
		where += ` AND ` + errCond
	}
	q := `SELECT ` + expr + ` AS v, COUNT(*), SUM(CASE WHEN ` + errCond + ` THEN 1 ELSE 0 END),
		COALESCE(SUM(cost_credits), 0), COALESCE(SUM(cost_micro_usd), 0), MAX(ts)
		FROM request_logs WHERE ` + where + ` GROUP BY v ORDER BY COUNT(*) DESC, v`
	args := []any{ms(from), ms(to)}
	if n > 0 {
		q += ` LIMIT ?`
		args = append(args, n)
	}
	rows, err := s.logs.r.QueryContext(ctx, s.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TopRow
	for rows.Next() {
		var (
			r    TopRow
			last int64
		)
		if err := rows.Scan(&r.Value, &r.Requests, &r.Errors, &r.Credits, &r.MicroUSD, &last); err != nil {
			return nil, err
		}
		r.LastTS = fromMS(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRequestLog returns one request log by ID.
func (s *Store) GetRequestLog(ctx context.Context, id int64) (RequestLog, error) {
	rows, err := s.QueryRequestLogs(ctx, LogQuery{BeforeID: id + 1, Limit: 1})
	if err != nil {
		return RequestLog{}, err
	}
	if len(rows) == 0 || rows[0].ID != id {
		return RequestLog{}, ErrNotFound
	}
	return rows[0], nil
}
