-- +goose Up
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '30s';
CREATE TABLE device_configurations (
 device_id uuid NOT NULL REFERENCES devices(id),
 version bigint NOT NULL,
 configuration jsonb NOT NULL,
 tariff text NOT NULL CHECK(tariff IN ('fixed','variable')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(device_id,version)
);
INSERT INTO device_configurations(device_id,version,configuration,tariff)
 SELECT d.id,(d.config->>'config_version')::bigint,d.config,COALESCE(p.tariff_type,'variable')
 FROM devices d LEFT JOIN building_profiles p ON p.building_id=d.building_id WHERE d.config IS NOT NULL;
ALTER TABLE commands ADD COLUMN configuration jsonb;

-- +goose Down
SET LOCAL lock_timeout = '3s';
-- squawk-ignore ban-drop-column
ALTER TABLE commands DROP COLUMN configuration;
-- squawk-ignore ban-drop-table
DROP TABLE device_configurations;
