-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE devices ADD COLUMN diagnostics jsonb;
ALTER TABLE devices ADD COLUMN manufacturer_failures bigint NOT NULL DEFAULT 0;

ALTER TABLE outbox ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();

-- +goose Down
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
-- squawk-ignore ban-drop-column
ALTER TABLE outbox DROP COLUMN created_at;
-- squawk-ignore ban-drop-column
ALTER TABLE devices DROP COLUMN diagnostics, DROP COLUMN manufacturer_failures;
