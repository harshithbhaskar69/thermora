-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE commands ADD COLUMN flexibility_event_id uuid;

-- +goose Down
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
-- squawk-ignore ban-drop-column
ALTER TABLE commands DROP COLUMN flexibility_event_id;
