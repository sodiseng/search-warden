package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Session is a server-side admin session. Only the SHA-256 of the session
// secret is stored (ARCHITECTURE.md §11.1).
type Session struct {
	IDHash     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time // absolute expiry; idle expiry is enforced by the auth layer from LastSeenAt
	IP         string
	UserAgent  string
}

// CreateSession inserts a new session.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	_, err := s.state.w.ExecContext(ctx, s.rebind(
		`INSERT INTO admin_sessions (id_hash, created_at, last_seen_at, expires_at, ip, user_agent) VALUES (?,?,?,?,?,?)`),
		sess.IDHash, ms(sess.CreatedAt), ms(sess.LastSeenAt), ms(sess.ExpiresAt), cleanText(sess.IP), cleanText(sess.UserAgent))
	return err
}

// GetSession returns ErrNotFound for unknown or absolutely-expired sessions.
func (s *Store) GetSession(ctx context.Context, idHash string, now time.Time) (Session, error) {
	var (
		sess               Session
		created, seen, exp int64
	)
	err := s.state.r.QueryRowContext(ctx, s.rebind(
		`SELECT id_hash, created_at, last_seen_at, expires_at, ip, user_agent FROM admin_sessions WHERE id_hash = ? AND expires_at > ?`),
		idHash, ms(now)).Scan(&sess.IDHash, &created, &seen, &exp, &sess.IP, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = fromMS(created), fromMS(seen), fromMS(exp)
	return sess, nil
}

// TouchSession records activity (sliding idle window).
func (s *Store) TouchSession(ctx context.Context, idHash string, seen time.Time) error {
	_, err := s.state.w.ExecContext(ctx, s.rebind(`UPDATE admin_sessions SET last_seen_at = ? WHERE id_hash = ?`), ms(seen), idHash)
	return err
}

// ListSessions returns live sessions, newest first.
func (s *Store) ListSessions(ctx context.Context, now time.Time) ([]Session, error) {
	rows, err := s.state.r.QueryContext(ctx, s.rebind(
		`SELECT id_hash, created_at, last_seen_at, expires_at, ip, user_agent FROM admin_sessions WHERE expires_at > ? ORDER BY created_at DESC`), ms(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var (
			sess               Session
			created, seen, exp int64
		)
		if err := rows.Scan(&sess.IDHash, &created, &seen, &exp, &sess.IP, &sess.UserAgent); err != nil {
			return nil, err
		}
		sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = fromMS(created), fromMS(seen), fromMS(exp)
		out = append(out, sess)
	}
	return out, rows.Err()
}

// DeleteSession revokes one session (logout).
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.state.w.ExecContext(ctx, s.rebind(`DELETE FROM admin_sessions WHERE id_hash = ?`), idHash)
	return err
}

// DeleteAllSessions revokes every session (password / TOTP change). Returns the count.
func (s *Store) DeleteAllSessions(ctx context.Context) (int64, error) {
	res, err := s.state.w.ExecContext(ctx, `DELETE FROM admin_sessions`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteExpiredSessions removes sessions past their absolute expiry or idle since before idleCutoff.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now, idleCutoff time.Time) (int64, error) {
	res, err := s.state.w.ExecContext(ctx, s.rebind(`DELETE FROM admin_sessions WHERE expires_at <= ? OR last_seen_at < ?`), ms(now), ms(idleCutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
