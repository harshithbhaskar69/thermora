-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
CREATE TABLE observed_replays (
 id uuid PRIMARY KEY,
 organization_id uuid NOT NULL,
 device_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 report jsonb NOT NULL,
 FOREIGN KEY (organization_id,device_id) REFERENCES devices(organization_id,id)
);
CREATE INDEX observed_replays_device_time ON observed_replays(device_id,created_at DESC);

-- +goose Down
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
-- squawk-ignore ban-drop-table
DROP TABLE observed_replays;
