-- Preserve the already-applied v1 serial sequences and integer fields. New
-- migrations keep these schema-preference checks enabled.
-- squawk-ignore-file prefer-bigint-over-int, prefer-identity
-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
CREATE TABLE organizations (id uuid PRIMARY KEY, name text NOT NULL);
CREATE TABLE users (id uuid PRIMARY KEY, login text UNIQUE NOT NULL, password_hash text NOT NULL);
CREATE TABLE memberships (
  organization_id uuid REFERENCES organizations(id), user_id uuid REFERENCES users(id),
  role text NOT NULL CHECK (role IN ('owner','operator')), PRIMARY KEY (organization_id,user_id)
);
CREATE TABLE sessions (token_hash bytea PRIMARY KEY, user_id uuid NOT NULL REFERENCES users(id), expires_at timestamptz NOT NULL);
CREATE INDEX sessions_expiry ON sessions(expires_at);
CREATE TABLE buildings (
  id uuid PRIMARY KEY, organization_id uuid NOT NULL REFERENCES organizations(id), name text NOT NULL,
  timezone text NOT NULL, comfort_target double precision NOT NULL DEFAULT 21,
  comfort_min double precision NOT NULL DEFAULT 20, comfort_max double precision NOT NULL DEFAULT 22,
  version integer NOT NULL DEFAULT 1, UNIQUE(organization_id,id),
  CHECK(comfort_min >= 16 AND comfort_max <= 26 AND comfort_max-comfort_min >= 0.5 AND comfort_target BETWEEN comfort_min AND comfort_max)
);
CREATE TABLE building_grants (
  organization_id uuid NOT NULL, building_id uuid NOT NULL, user_id uuid NOT NULL,
  PRIMARY KEY(organization_id,building_id,user_id),
  FOREIGN KEY(organization_id,building_id) REFERENCES buildings(organization_id,id),
  FOREIGN KEY(organization_id,user_id) REFERENCES memberships(organization_id,user_id)
);
CREATE TABLE devices (
  id uuid PRIMARY KEY, organization_id uuid NOT NULL REFERENCES organizations(id), building_id uuid,
  name text NOT NULL DEFAULT 'Heat pump 01', enrollment text NOT NULL DEFAULT 'inventory'
    CHECK(enrollment IN ('inventory','claimed','activating','active','revoked')),
  claim_hash bytea UNIQUE, claim_expires_at timestamptz,
  bootstrap_hash bytea NOT NULL, bootstrap_expires_at timestamptz NOT NULL,
  activation_id uuid, activation_digest bytea, activated_at timestamptz, credential_hash bytea,
  run_id uuid NOT NULL, run_epoch bigint NOT NULL CHECK(run_epoch BETWEEN 1 AND 4294967295),
  config jsonb, accepted_version bigint NOT NULL DEFAULT 0, reported_version bigint NOT NULL DEFAULT 0,
  command_seq bigint NOT NULL DEFAULT 0 CHECK(command_seq BETWEEN 0 AND 4294967295),
  latest jsonb, latest_elapsed bigint NOT NULL DEFAULT -1, last_received_at timestamptz,
  override_target double precision, override_until timestamptz,
  UNIQUE(organization_id,id), FOREIGN KEY(organization_id,building_id) REFERENCES buildings(organization_id,id)
);
CREATE TABLE telemetry_batches (
  organization_id uuid NOT NULL, device_id uuid NOT NULL, batch_id uuid NOT NULL, digest bytea NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(organization_id,device_id,batch_id),
  FOREIGN KEY(organization_id,device_id) REFERENCES devices(organization_id,id)
);
CREATE TABLE telemetry (
  organization_id uuid NOT NULL, device_id uuid NOT NULL, run_id uuid NOT NULL, boot_id uuid NOT NULL,
  sample_seq bigint NOT NULL, elapsed bigint NOT NULL, observed_at timestamptz NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(), sample jsonb NOT NULL, digest bytea NOT NULL,
  PRIMARY KEY(organization_id,device_id,run_id,boot_id,sample_seq),
  FOREIGN KEY(organization_id,device_id) REFERENCES devices(organization_id,id)
);
CREATE INDEX telemetry_device_time ON telemetry(organization_id,device_id,run_id,elapsed DESC);
CREATE TABLE commands (
  id uuid PRIMARY KEY, organization_id uuid NOT NULL, device_id uuid NOT NULL,
  payload jsonb NOT NULL, stage text NOT NULL DEFAULT 'persisted', stage_rank integer NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(organization_id,id),
  FOREIGN KEY(organization_id,device_id) REFERENCES devices(organization_id,id)
);
CREATE INDEX commands_device_time ON commands(organization_id,device_id,created_at DESC);
CREATE TABLE command_events (
  id uuid PRIMARY KEY, organization_id uuid NOT NULL, command_id uuid NOT NULL,
  payload jsonb NOT NULL, received_at timestamptz NOT NULL DEFAULT now(),
  FOREIGN KEY(organization_id,command_id) REFERENCES commands(organization_id,id)
);
CREATE TABLE outbox (
  id bigserial PRIMARY KEY, organization_id uuid NOT NULL, device_id uuid NOT NULL,
  topic text NOT NULL, payload jsonb NOT NULL, retained boolean NOT NULL DEFAULT false,
  command_id uuid REFERENCES commands(id), attempts integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz,
  FOREIGN KEY(organization_id,device_id) REFERENCES devices(organization_id,id)
);
CREATE INDEX outbox_due ON outbox(next_attempt_at);
CREATE TABLE audit (
  id bigserial PRIMARY KEY, organization_id uuid NOT NULL REFERENCES organizations(id), actor_id uuid,
  action text NOT NULL, resource_id uuid NOT NULL, request_id text NOT NULL,
  recorded_at timestamptz NOT NULL DEFAULT now(), detail jsonb NOT NULL DEFAULT '{}'
);
CREATE TABLE idempotency (
  actor_id uuid NOT NULL, operation text NOT NULL, key text NOT NULL, digest bytea NOT NULL,
  response jsonb NOT NULL, expires_at timestamptz NOT NULL DEFAULT now()+interval '24 hours',
  PRIMARY KEY(actor_id,operation,key)
);

-- +goose Down
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
-- Explicit rollback of the entire initial schema, never an online upgrade.
-- squawk-ignore ban-drop-table
DROP TABLE idempotency,audit,outbox,command_events,commands,telemetry,telemetry_batches,devices,building_grants,buildings,sessions,memberships,users,organizations;
