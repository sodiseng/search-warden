package store

import (
	"context"
	"time"
)

// TokenUsage is one token's usage in one period ("d2026-11-10" or "m2026-11", UTC).
type TokenUsage struct {
	TokenID  string
	Period   string
	Requests int64
	Credits  int64
	MicroUSD int64
}

// UpsertTokenUsage writes absolute counters (the limiter in memory is authoritative).
func (s *Store) UpsertTokenUsage(ctx context.Context, rows []TokenUsage) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.state.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, s.rebind(`INSERT INTO token_usage (token_id, period, requests, credits, micro_usd, updated_at)
		VALUES (?,?,?,?,?,?) ON CONFLICT (token_id, period) DO UPDATE SET requests = excluded.requests,
		credits = excluded.credits, micro_usd = excluded.micro_usd, updated_at = excluded.updated_at`))
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UnixMilli()
	for _, u := range rows {
		if _, err := stmt.ExecContext(ctx, u.TokenID, u.Period, u.Requests, u.Credits, u.MicroUSD, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListTokenUsage returns the counters of the given periods.
func (s *Store) ListTokenUsage(ctx context.Context, periods ...string) ([]TokenUsage, error) {
	var out []TokenUsage
	for _, p := range periods {
		rows, err := s.state.r.QueryContext(ctx, s.rebind(`SELECT token_id, period, requests, credits, micro_usd FROM token_usage WHERE period = ?`), p)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var u TokenUsage
			if err := rows.Scan(&u.TokenID, &u.Period, &u.Requests, &u.Credits, &u.MicroUSD); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, u)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PruneTokenUsage deletes counters not updated since cutoff.
func (s *Store) PruneTokenUsage(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.state.w.ExecContext(ctx, s.rebind(`DELETE FROM token_usage WHERE updated_at < ?`), ms(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
