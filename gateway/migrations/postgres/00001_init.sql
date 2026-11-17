-- PostgreSQL schema: runtime state + request logs in one database.
-- Times are Unix milliseconds (BIGINT) to keep the SQL identical to SQLite where possible.
-- request_logs is range-partitioned by day on ts; the gateway creates upcoming partitions
-- and drops expired ones (internal/store: EnsureLogPartitions / PurgeRequestLogsBefore).

-- +goose Up
CREATE TABLE admin_sessions (
    id_hash      TEXT   PRIMARY KEY,
    created_at   BIGINT NOT NULL,
    last_seen_at BIGINT NOT NULL,
    expires_at   BIGINT NOT NULL,
    ip           TEXT   NOT NULL DEFAULT '',
    user_agent   TEXT   NOT NULL DEFAULT ''
);
CREATE INDEX admin_sessions_expires ON admin_sessions (expires_at);

CREATE TABLE audit_log (
    id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts     BIGINT NOT NULL,
    source TEXT   NOT NULL,
    ip     TEXT   NOT NULL DEFAULT '',
    action TEXT   NOT NULL,
    target TEXT   NOT NULL DEFAULT '',
    diff   TEXT   NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_ts ON audit_log (ts);

CREATE TABLE key_state (
    key_id          TEXT             PRIMARY KEY,
    provider        TEXT             NOT NULL,
    status          TEXT             NOT NULL DEFAULT 'active',
    status_until    BIGINT           NOT NULL DEFAULT 0,
    status_reason   TEXT             NOT NULL DEFAULT '',
    fail_score      DOUBLE PRECISION NOT NULL DEFAULT 0,
    quota_limit     BIGINT,
    quota_used      BIGINT,
    quota_remaining BIGINT,
    quota_unit      TEXT             NOT NULL DEFAULT '',
    period_reset_at BIGINT           NOT NULL DEFAULT 0,
    last_synced_at  BIGINT           NOT NULL DEFAULT 0,
    last_used_at    BIGINT           NOT NULL DEFAULT 0,
    removed_at      BIGINT           NOT NULL DEFAULT 0,
    updated_at      BIGINT           NOT NULL
);
CREATE INDEX key_state_provider ON key_state (provider);

CREATE TABLE egress_state (
    egress_id         TEXT   PRIMARY KEY,
    exit_ip           TEXT   NOT NULL DEFAULT '',
    exit_country      TEXT   NOT NULL DEFAULT '',
    health            TEXT   NOT NULL DEFAULT 'unknown',
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    fail_streak       INTEGER NOT NULL DEFAULT 0,
    quarantined_until BIGINT NOT NULL DEFAULT 0,
    last_checked_at   BIGINT NOT NULL DEFAULT 0,
    last_error        TEXT   NOT NULL DEFAULT '',
    updated_at        BIGINT NOT NULL
);

-- An explicit sequence works on partitioned tables in every supported PostgreSQL version.
CREATE SEQUENCE request_logs_id_seq;
CREATE TABLE request_logs (
    id                  BIGINT  NOT NULL DEFAULT nextval('request_logs_id_seq'),
    ts                  BIGINT  NOT NULL,
    request_id          TEXT    NOT NULL,
    client_ip           TEXT    NOT NULL DEFAULT '',
    ip_source           TEXT    NOT NULL DEFAULT '',
    user_agent          TEXT    NOT NULL DEFAULT '',
    token_id            TEXT    NOT NULL DEFAULT '',
    provider            TEXT    NOT NULL DEFAULT '',
    surface             TEXT    NOT NULL DEFAULT '',
    operation           TEXT    NOT NULL DEFAULT '',
    key_id              TEXT    NOT NULL DEFAULT '',
    egress_id           TEXT    NOT NULL DEFAULT '',
    exit_ip             TEXT    NOT NULL DEFAULT '',
    attempts            INTEGER NOT NULL DEFAULT 0,
    status              INTEGER NOT NULL DEFAULT 0,
    outcome             TEXT    NOT NULL DEFAULT '',
    error_kind          TEXT    NOT NULL DEFAULT '',
    latency_ms          INTEGER NOT NULL DEFAULT 0,
    upstream_latency_ms INTEGER NOT NULL DEFAULT 0,
    cost_credits        BIGINT  NOT NULL DEFAULT 0,
    cost_micro_usd      BIGINT  NOT NULL DEFAULT 0,
    cost_estimated      BOOLEAN NOT NULL DEFAULT FALSE,
    req_bytes           BIGINT  NOT NULL DEFAULT 0,
    resp_bytes          BIGINT  NOT NULL DEFAULT 0,
    query_preview       TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (ts, id)
) PARTITION BY RANGE (ts);
ALTER SEQUENCE request_logs_id_seq OWNED BY request_logs.id;
-- Catches rows outside the pre-created daily partitions (e.g. after downtime); purged row by row.
CREATE TABLE request_logs_default PARTITION OF request_logs DEFAULT;
-- (ts, id) primary key already serves time-range scans.
CREATE INDEX request_logs_key_ts   ON request_logs (key_id, ts);
CREATE INDEX request_logs_token_ts ON request_logs (token_id, ts);
CREATE INDEX request_logs_id       ON request_logs (id);

CREATE TABLE request_attempts (
    request_id TEXT    NOT NULL,
    n          INTEGER NOT NULL,
    ts         BIGINT  NOT NULL,
    key_id     TEXT    NOT NULL DEFAULT '',
    egress_id  TEXT    NOT NULL DEFAULT '',
    status     INTEGER NOT NULL DEFAULT 0,
    error_kind TEXT    NOT NULL DEFAULT '',
    latency_ms INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (request_id, n)
);
CREATE INDEX request_attempts_ts ON request_attempts (ts);

-- +goose Down
DROP TABLE request_attempts;
DROP TABLE request_logs;
DROP TABLE egress_state;
DROP TABLE key_state;
DROP TABLE audit_log;
DROP TABLE admin_sessions;
