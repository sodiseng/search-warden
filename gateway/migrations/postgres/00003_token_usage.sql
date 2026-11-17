-- Per-token usage counters for token limits (ARCHITECTURE.md §6.4, §10.1).

-- +goose Up
CREATE TABLE token_usage (
    token_id   TEXT   NOT NULL,
    period     TEXT   NOT NULL,
    requests   BIGINT NOT NULL DEFAULT 0,
    credits    BIGINT NOT NULL DEFAULT 0,
    micro_usd  BIGINT NOT NULL DEFAULT 0,
    updated_at BIGINT NOT NULL,
    PRIMARY KEY (token_id, period)
);

-- +goose Down
DROP TABLE token_usage;
