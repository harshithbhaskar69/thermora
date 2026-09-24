-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
CREATE TABLE flexibility_events (
 id uuid PRIMARY KEY,
 organization_id uuid NOT NULL REFERENCES organizations(id),
 status text NOT NULL CHECK(status IN ('active','completed','cancelled','timed_out')),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 duration_minutes bigint NOT NULL CHECK(duration_minutes BETWEEN 5 AND 60),
 UNIQUE(organization_id,id)
);
CREATE UNIQUE INDEX one_active_flexibility_event ON flexibility_events(organization_id) WHERE status='active';
CREATE TABLE flexibility_participants (
 event_id uuid NOT NULL,
 organization_id uuid NOT NULL,
 device_id uuid NOT NULL,
 run_id uuid NOT NULL,
 start_sim timestamptz NOT NULL,
 end_sim timestamptz NOT NULL,
 status text NOT NULL CHECK(status IN ('eligible','excluded','completed')),
 reason text NOT NULL,
 reference_power_kw double precision,
 PRIMARY KEY(event_id,device_id),
 FOREIGN KEY(organization_id,event_id) REFERENCES flexibility_events(organization_id,id),
 FOREIGN KEY(organization_id,device_id) REFERENCES devices(organization_id,id)
);
CREATE INDEX flexibility_participants_device ON flexibility_participants(device_id) WHERE status='eligible';

-- +goose Down
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
-- squawk-ignore ban-drop-table
DROP TABLE flexibility_participants,flexibility_events;
