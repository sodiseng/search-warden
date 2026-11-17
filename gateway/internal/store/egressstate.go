package store

import (
	"context"
	"time"
)

// EgressState is the runtime health of one egress, keyed by the ID derived
// from its normalized URL (internal/egress.ID), so renaming a node keeps it.
type EgressState struct {
	EgressID         string
	ExitIP           string
	ExitCountry      string
	Health           string // unknown | healthy | degraded | quarantined | blocked
	LatencyMS        int
	FailStreak       int
	QuarantinedUntil time.Time
	LastCheckedAt    time.Time
	LastError        string // must not contain credentials
	UpdatedAt        time.Time
}

// UpsertEgressStates writes a batch in one transaction.
func (s *Store) UpsertEgressStates(ctx context.Context, states []EgressState) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.state.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, s.rebind(`INSERT INTO egress_state
		(egress_id, exit_ip, exit_country, health, latency_ms, fail_streak, quarantined_until, last_checked_at, last_error, updated_at)
		VALUES `+placeholders(10)+`
		ON CONFLICT (egress_id) DO UPDATE SET
			exit_ip = excluded.exit_ip, exit_country = excluded.exit_country, health = excluded.health,
			latency_ms = excluded.latency_ms, fail_streak = excluded.fail_streak,
			quarantined_until = excluded.quarantined_until, last_checked_at = excluded.last_checked_at,
			last_error = excluded.last_error, updated_at = excluded.updated_at`))
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now()
	for _, e := range states {
		upd := e.UpdatedAt
		if upd.IsZero() {
			upd = now
		}
		health := e.Health
		if health == "" {
			health = "unknown"
		}
		if _, err := stmt.ExecContext(ctx, e.EgressID, e.ExitIP, e.ExitCountry, health, e.LatencyMS, e.FailStreak,
			ms(e.QuarantinedUntil), ms(e.LastCheckedAt), e.LastError, ms(upd)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListEgressStates returns all egress states keyed by egress ID.
func (s *Store) ListEgressStates(ctx context.Context) (map[string]EgressState, error) {
	rows, err := s.state.r.QueryContext(ctx, `SELECT egress_id, exit_ip, exit_country, health, latency_ms, fail_streak,
		quarantined_until, last_checked_at, last_error, updated_at FROM egress_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]EgressState{}
	for rows.Next() {
		var (
			e                  EgressState
			quar, checked, upd int64
		)
		if err := rows.Scan(&e.EgressID, &e.ExitIP, &e.ExitCountry, &e.Health, &e.LatencyMS, &e.FailStreak,
			&quar, &checked, &e.LastError, &upd); err != nil {
			return nil, err
		}
		e.QuarantinedUntil, e.LastCheckedAt, e.UpdatedAt = fromMS(quar), fromMS(checked), fromMS(upd)
		out[e.EgressID] = e
	}
	return out, rows.Err()
}

// DeleteEgressStatesExcept drops state for egresses no longer referenced by any node or key.
func (s *Store) DeleteEgressStatesExcept(ctx context.Context, keep []string) (int64, error) {
	existing, err := s.ListEgressStates(ctx)
	if err != nil {
		return 0, err
	}
	keepSet := make(map[string]bool, len(keep))
	for _, id := range keep {
		keepSet[id] = true
	}
	var n int64
	for id := range existing {
		if keepSet[id] {
			continue
		}
		res, err := s.state.w.ExecContext(ctx, s.rebind(`DELETE FROM egress_state WHERE egress_id = ?`), id)
		if err != nil {
			return n, err
		}
		c, _ := res.RowsAffected()
		n += c
	}
	return n, nil
}
