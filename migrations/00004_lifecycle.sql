-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
ALTER TABLE cloud_enrollments ADD COLUMN control_connected boolean NOT NULL DEFAULT true;
-- Records when a grant was withdrawn so an older in-flight OAuth flow cannot revive it.
ALTER TABLE manufacturer_connections ADD COLUMN revoked_at timestamptz;
ALTER TABLE manufacturer_connections ADD COLUMN revocation_pending boolean NOT NULL DEFAULT false;
ALTER TABLE manufacturer_authorizations ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();

-- +goose Down
SET LOCAL lock_timeout = '3s';
-- squawk-ignore ban-drop-column
ALTER TABLE manufacturer_authorizations DROP COLUMN created_at;
-- squawk-ignore ban-drop-column
ALTER TABLE manufacturer_connections DROP COLUMN revoked_at;
-- squawk-ignore ban-drop-column
ALTER TABLE manufacturer_connections DROP COLUMN revocation_pending;
-- squawk-ignore ban-drop-column
ALTER TABLE cloud_enrollments DROP COLUMN control_connected;
