-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE devices ADD COLUMN credential_revocation_pending boolean NOT NULL DEFAULT false;

-- +goose Down
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
-- squawk-ignore ban-drop-column
ALTER TABLE devices DROP COLUMN credential_revocation_pending;
