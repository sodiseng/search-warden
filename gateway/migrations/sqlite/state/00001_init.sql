-- Runtime state (SQLite state.db). Times are Unix milliseconds; 0 means "unset".
-- Configuration (keys, egresses, tokens, settings) lives in YAML files, not here.

-- +goose Up
CREATE TABLE admin_sessions (
    id_hash      TEXT    PRIMARY KEY,          -- sha256 of the session secret; the secret itself is never stored
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,             -- absolute expiry
    ip           TEXT    NOT NULL DEFAULT '',
    user_agent   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX admin_sessions_expires ON admin_sessions (expires_at);

CREATE TABLE audit_log (
    id     INTEGER PRIMARY KEY,                -- rowid alias, monotonically increasing
    ts     INTEGER NOT NULL,
    source TEXT    NOT NULL,                   -- panel | file | system | cli
    ip     TEXT    NOT NULL DEFAULT '',
    action TEXT    NOT NULL,
    target TEXT    NOT NULL DEFAULT '',
    diff   TEXT    NOT NULL DEFAULT ''         -- secrets redacted before insert
);
CREATE INDEX audit_log_ts ON audit_log (ts);

CREATE TABLE key_state (
    key_id          TEXT    PRIMARY KEY,       -- first 6 bytes (12 hex chars) of sha256(provider + ":" + key)
    provider        TEXT    NOT NULL,
    status          TEXT    NOT NULL DEFAULT 'active',
    status_until    INTEGER NOT NULL DEFAULT 0,
    status_reason   TEXT    NOT NULL DEFAULT '',
    fail_score      REAL    NOT NULL DEFAULT 0,
    quota_limit     INTEGER,                   -- NULL = unknown
    quota_used      INTEGER,
    quota_remaining INTEGER,
    quota_unit      TEXT    NOT NULL DEFAULT '',   -- credits | micro_usd
    period_reset_at INTEGER NOT NULL DEFAULT 0,
    last_synced_at  INTEGER NOT NULL DEFAULT 0,
    last_used_at    INTEGER NOT NULL DEFAULT 0,
    removed_at      INTEGER NOT NULL DEFAULT 0,    -- key deleted from keys/*.yaml; purged after 30 days
    updated_at      INTEGER NOT NULL
);
CREATE INDEX key_state_provider ON key_state (provider);

CREATE TABLE egress_state (
    egress_id         TEXT    PRIMARY KEY,     -- derived from the normalized proxy URL (see internal/egress)
    exit_ip           TEXT    NOT NULL DEFAULT '',
    exit_country      TEXT    NOT NULL DEFAULT '',
    health            TEXT    NOT NULL DEFAULT 'unknown',  -- unknown | healthy | degraded | quarantined | blocked
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    fail_streak       INTEGER NOT NULL DEFAULT 0,
    quarantined_until INTEGER NOT NULL DEFAULT 0,
    last_checked_at   INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT    NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL
);

-- +goose Down
DROP TABLE egress_state;
DROP TABLE key_state;
DROP TABLE audit_log;
DROP TABLE admin_sessions;
