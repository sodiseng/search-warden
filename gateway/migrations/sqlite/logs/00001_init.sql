-- Request logs (SQLite logs.db). Times are Unix milliseconds.
-- Retention: rows older than logs.retention_days (default 30) are deleted daily in batches,
-- followed by PRAGMA incremental_vacuum (auto_vacuum=INCREMENTAL is set when the file is created).

-- +goose Up
CREATE TABLE request_logs (
    id                  INTEGER PRIMARY KEY,
    ts                  INTEGER NOT NULL,
    request_id          TEXT    NOT NULL,
    client_ip           TEXT    NOT NULL DEFAULT '',
    ip_source           TEXT    NOT NULL DEFAULT '',
    user_agent          TEXT    NOT NULL DEFAULT '',   -- truncated to 64 chars
    token_id            TEXT    NOT NULL DEFAULT '',
    provider            TEXT    NOT NULL DEFAULT '',
    surface             TEXT    NOT NULL DEFAULT '',   -- mcp | rest
    operation           TEXT    NOT NULL DEFAULT '',   -- tool name or REST endpoint
    key_id              TEXT    NOT NULL DEFAULT '',
    egress_id           TEXT    NOT NULL DEFAULT '',
    exit_ip             TEXT    NOT NULL DEFAULT '',
    attempts            INTEGER NOT NULL DEFAULT 0,
    status              INTEGER NOT NULL DEFAULT 0,
    outcome             TEXT    NOT NULL DEFAULT '',
    error_kind          TEXT    NOT NULL DEFAULT '',
    latency_ms          INTEGER NOT NULL DEFAULT 0,
    upstream_latency_ms INTEGER NOT NULL DEFAULT 0,
    cost_credits        INTEGER NOT NULL DEFAULT 0,
    cost_micro_usd      INTEGER NOT NULL DEFAULT 0,
    cost_estimated      INTEGER NOT NULL DEFAULT 0,    -- boolean
    req_bytes           INTEGER NOT NULL DEFAULT 0,
    resp_bytes          INTEGER NOT NULL DEFAULT 0,
    query_preview       TEXT    NOT NULL DEFAULT ''    -- first 40 characters (configurable, 0 = off)
);
CREATE INDEX request_logs_ts       ON request_logs (ts);
CREATE INDEX request_logs_key_ts   ON request_logs (key_id, ts);
CREATE INDEX request_logs_token_ts ON request_logs (token_id, ts);

-- Only written when a request needed more than one attempt.
CREATE TABLE request_attempts (
    request_id TEXT    NOT NULL,
    n          INTEGER NOT NULL,
    ts         INTEGER NOT NULL,
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
