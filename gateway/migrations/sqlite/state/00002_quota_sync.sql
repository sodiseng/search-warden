-- Quota sync state (ARCHITECTURE.md §10.2), kept with the rest of the key state.

-- +goose Up
ALTER TABLE key_state ADD COLUMN quota_plan   TEXT    NOT NULL DEFAULT '';  -- plan name reported upstream (Tavily current_plan)
ALTER TABLE key_state ADD COLUMN sync_error   TEXT    NOT NULL DEFAULT '';  -- last quota sync error; '' after a successful sync
ALTER TABLE key_state ADD COLUMN next_sync_at INTEGER NOT NULL DEFAULT 0;   -- earliest next sync, so gaps and backoff survive restarts

-- +goose Down
ALTER TABLE key_state DROP COLUMN next_sync_at;
ALTER TABLE key_state DROP COLUMN sync_error;
ALTER TABLE key_state DROP COLUMN quota_plan;
