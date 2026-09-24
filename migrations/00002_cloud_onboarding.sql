-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';

CREATE TABLE manufacturer_authorizations (
    state_hash bytea PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    browser_hash bytea NOT NULL,
    verifier bytea NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX manufacturer_authorizations_user ON manufacturer_authorizations(user_id);

CREATE TABLE manufacturer_connections (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users(id),
    provider text NOT NULL CHECK (provider = 'demo'),
    subject text NOT NULL,
    tokens bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    status text NOT NULL DEFAULT 'connected' CHECK (status IN ('connected', 'authentication_required', 'disconnected')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(user_id, provider, subject)
);

-- Local mock discovery maps only explicitly provisioned inventory, never arbitrary device IDs.
CREATE TABLE manufacturer_inventory (
    device_id uuid PRIMARY KEY REFERENCES devices(id),
    provider text NOT NULL CHECK (provider = 'demo'),
    subject text NOT NULL,
    external_id text NOT NULL,
    UNIQUE(provider, subject, external_id)
);

CREATE TABLE partner_invitations (
    code_hash bytea PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations(id),
    expires_at timestamptz NOT NULL,
    redeemed_by uuid REFERENCES users(id),
    redeemed_at timestamptz
);

CREATE TABLE cloud_enrollments (
    device_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    connection_id uuid NOT NULL REFERENCES manufacturer_connections(id),
    consent_version text NOT NULL,
    consented_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (organization_id, device_id) REFERENCES devices(organization_id, id)
);
CREATE INDEX cloud_enrollments_connection ON cloud_enrollments(connection_id);

CREATE TABLE building_profiles (
    building_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    floor_area_m2 double precision NOT NULL CHECK (floor_area_m2 > 0 AND floor_area_m2 <= 10000),
    country_code text NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
    address text NOT NULL,
    postal_code text NOT NULL,
    city text NOT NULL,
    tariff_type text NOT NULL CHECK (tariff_type IN ('fixed', 'variable')),
    FOREIGN KEY (organization_id, building_id) REFERENCES buildings(organization_id, id)
);

-- +goose Down
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
-- Explicit rollback of this local onboarding schema, never an online upgrade.
-- squawk-ignore ban-drop-table
DROP TABLE building_profiles,cloud_enrollments,partner_invitations,manufacturer_inventory,manufacturer_connections,manufacturer_authorizations;
