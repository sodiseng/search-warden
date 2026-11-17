package store

import (
	"context"
	"database/sql"
	"time"
)

// KeyState is the persisted runtime state of one upstream key. The key pool
// keeps the authoritative copy in memory and flushes changes in batches
// (ARCHITECTURE.md §6.1), so writes here are bulk upserts.
type KeyState struct {
	KeyID          string
	Provider       string
	Status         string // active | cooldown | exhausted | invalid | disabled
	StatusUntil    time.Time
	StatusReason   string
	FailScore      float64
	QuotaLimit     *int64 // nil = unknown
	QuotaUsed      *int64
	QuotaRemaining *int64
	QuotaUnit      string // credits | micro_usd
	PeriodResetAt  time.Time
	LastSyncedAt   time.Time
	LastUsedAt     time.Time
	RemovedAt      time.Time // set when the key disappears from keys/*.yaml
	UpdatedAt      time.Time
	QuotaPlan      string    // plan name reported upstream (Tavily current_plan)
	SyncError      string    // last quota sync error; "" after a successful sync
	NextSyncAt     time.Time // earliest next quota sync (gap / backoff)
}

const keyStateCols = `key_id, provider, status, status_until, status_reason, fail_score,
	quota_limit, quota_used, quota_remaining, quota_unit, period_reset_at, last_synced_at,
	last_used_at, removed_at, updated_at, quota_plan, sync_error, next_sync_at`

// UpsertKeyStates writes a batch in one transaction.
func (s *Store) UpsertKeyStates(ctx context.Context, states []KeyState) error {
	if len(states) == 0 {
		return nil
	}
	tx, err := s.state.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, s.rebind(`INSERT INTO key_state (`+keyStateCols+`) VALUES `+placeholders(18)+`
		ON CONFLICT (key_id) DO UPDATE SET
			provider = excluded.provider, status = excluded.status, status_until = excluded.status_until,
			status_reason = excluded.status_reason, fail_score = excluded.fail_score,
			quota_limit = excluded.quota_limit, quota_used = excluded.quota_used,
			quota_remaining = excluded.quota_remaining, quota_unit = excluded.quota_unit,
			period_reset_at = excluded.period_reset_at, last_synced_at = excluded.last_synced_at,
			last_used_at = excluded.last_used_at, removed_at = excluded.removed_at, updated_at = excluded.updated_at,
			quota_plan = excluded.quota_plan, sync_error = excluded.sync_error, next_sync_at = excluded.next_sync_at`))
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now()
	for _, k := range states {
		upd := k.UpdatedAt
		if upd.IsZero() {
			upd = now
		}
		if _, err := stmt.ExecContext(ctx, k.KeyID, k.Provider, k.Status, ms(k.StatusUntil), k.StatusReason, k.FailScore,
			nullInt(k.QuotaLimit), nullInt(k.QuotaUsed), nullInt(k.QuotaRemaining), k.QuotaUnit,
			ms(k.PeriodResetAt), ms(k.LastSyncedAt), ms(k.LastUsedAt), ms(k.RemovedAt), ms(upd),
			k.QuotaPlan, cleanText(k.SyncError), ms(k.NextSyncAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListKeyStates returns all states, optionally filtered by provider ("" = all).
func (s *Store) ListKeyStates(ctx context.Context, provider string) ([]KeyState, error) {
	q := `SELECT ` + keyStateCols + ` FROM key_state`
	var args []any
	if provider != "" {
		q += ` WHERE provider = ?`
		args = append(args, provider)
	}
	q += ` ORDER BY key_id`
	rows, err := s.state.r.QueryContext(ctx, s.rebind(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyState
	for rows.Next() {
		var (
			k                                                    KeyState
			until, reset, synced, used, removed, updated, nextAt int64
			qLimit, qUsed, qRemaining                            sql.NullInt64
		)
		if err := rows.Scan(&k.KeyID, &k.Provider, &k.Status, &until, &k.StatusReason, &k.FailScore,
			&qLimit, &qUsed, &qRemaining, &k.QuotaUnit, &reset, &synced, &used, &removed, &updated,
			&k.QuotaPlan, &k.SyncError, &nextAt); err != nil {
			return nil, err
		}
		k.StatusUntil, k.PeriodResetAt, k.LastSyncedAt = fromMS(until), fromMS(reset), fromMS(synced)
		k.LastUsedAt, k.RemovedAt, k.UpdatedAt, k.NextSyncAt = fromMS(used), fromMS(removed), fromMS(updated), fromMS(nextAt)
		k.QuotaLimit, k.QuotaUsed, k.QuotaRemaining = ptrInt(qLimit), ptrInt(qUsed), ptrInt(qRemaining)
		out = append(out, k)
	}
	return out, rows.Err()
}

// MarkKeysRemoved flags keys that are no longer in keys/*.yaml. Their state is
// kept (history still shows labels; re-adding a key restores it) until PurgeRemovedKeys.
func (s *Store) MarkKeysRemoved(ctx context.Context, keyIDs []string, at time.Time) error {
	return s.updateKeyIDs(ctx, `UPDATE key_state SET removed_at = ?, updated_at = ? WHERE removed_at = 0 AND key_id = ?`, keyIDs, ms(at))
}

// RestoreKeys clears the removed flag (the key reappeared in a file).
func (s *Store) RestoreKeys(ctx context.Context, keyIDs []string) error {
	return s.updateKeyIDs(ctx, `UPDATE key_state SET removed_at = 0, updated_at = ? WHERE removed_at <> 0 AND key_id = ?`, keyIDs)
}

func (s *Store) updateKeyIDs(ctx context.Context, q string, keyIDs []string, leading ...int64) error {
	if len(keyIDs) == 0 {
		return nil
	}
	tx, err := s.state.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, s.rebind(q))
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UnixMilli()
	for _, id := range keyIDs {
		args := make([]any, 0, len(leading)+2)
		for _, v := range leading {
			args = append(args, v)
		}
		args = append(args, now, id)
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PurgeRemovedKeys deletes state of keys removed before cutoff (default policy: 30 days).
func (s *Store) PurgeRemovedKeys(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.state.w.ExecContext(ctx, s.rebind(`DELETE FROM key_state WHERE removed_at <> 0 AND removed_at < ?`), ms(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func nullInt(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

func ptrInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}
