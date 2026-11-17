-- Quota sync state (ARCHITECTURE.md §10.2), kept with the rest of the key state.

-- +goose Up
ALTER TABLE key_state ADD COLUMN quota_plan   TEXT   NOT NULL DEFAULT '';
ALTER TABLE key_state ADD COLUMN sync_error   TEXT   NOT NULL DEFAULT '';
ALTER TABLE key_state ADD COLUMN next_sync_at BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE key_state DROP COLUMN next_sync_at;
ALTER TABLE key_state DROP COLUMN sync_error;
ALTER TABLE key_state DROP COLUMN quota_plan;
