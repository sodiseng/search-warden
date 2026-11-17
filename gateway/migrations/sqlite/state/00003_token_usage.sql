-- Per-token usage counters for token limits (ARCHITECTURE.md §6.4, §10.1).
-- period: 'd2026-11-10' (UTC day) or 'm2026-11' (UTC month). Absolute values,
-- written in batches by the in-memory limiter; old periods are pruned.

-- +goose Up
CREATE TABLE token_usage (
    token_id   TEXT    NOT NULL,
    period     TEXT    NOT NULL,
    requests   INTEGER NOT NULL DEFAULT 0,
    credits    INTEGER NOT NULL DEFAULT 0,
    micro_usd  INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (token_id, period)
);

-- +goose Down
DROP TABLE token_usage;
