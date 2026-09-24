-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
CREATE TABLE simulation_runs (
 id uuid PRIMARY KEY,
 device_id uuid NOT NULL REFERENCES devices(id),
 epoch bigint NOT NULL,
 rate bigint NOT NULL DEFAULT 60 CHECK (rate IN (1,10,60)),
 paused boolean NOT NULL DEFAULT false,
 model text NOT NULL DEFAULT 'heating' CHECK (model IN ('heating','heating-dhw')),
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 UNIQUE(device_id,epoch)
);
INSERT INTO simulation_runs(id,device_id,epoch) SELECT run_id,id,run_epoch FROM devices;
CREATE TABLE run_operations (
 id uuid PRIMARY KEY,
 device_id uuid NOT NULL REFERENCES devices(id),
 run_id uuid NOT NULL REFERENCES simulation_runs(id),
 sequence bigint NOT NULL,
 payload jsonb NOT NULL,
 completed_at timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(device_id,sequence)
);
CREATE UNIQUE INDEX run_operations_pending ON run_operations(device_id) WHERE completed_at IS NULL;

-- +goose Down
SET LOCAL lock_timeout = '3s';
-- squawk-ignore ban-drop-table
DROP TABLE run_operations,simulation_runs;
