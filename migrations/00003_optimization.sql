-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE optimization_plans (
    device_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    run_id uuid NOT NULL,
    config_version bigint NOT NULL,
    valid_from timestamptz NOT NULL,
    valid_until timestamptz NOT NULL,
    plan jsonb NOT NULL,
    FOREIGN KEY (organization_id, device_id) REFERENCES devices(organization_id, id),
    CHECK (valid_until > valid_from)
);

CREATE TABLE comparisons (
    id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    device_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    report jsonb NOT NULL,
    FOREIGN KEY (organization_id, device_id) REFERENCES devices(organization_id, id)
);
CREATE INDEX comparisons_device_time ON comparisons(organization_id, device_id, created_at DESC);

-- +goose Down
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
-- Deliberate rollback removes only this phase's model outputs, never device telemetry.
-- squawk-ignore ban-drop-table
DROP TABLE comparisons, optimization_plans;
