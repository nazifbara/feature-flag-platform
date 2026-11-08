-- +goose Up
-- A flag's settings in one environment.
CREATE TABLE flag_settings (
    id UUID PRIMARY KEY,
    flag_id UUID NOT NULL REFERENCES flags (id) ON DELETE CASCADE,
    environment_id UUID NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    enabled BOOLEAN NOT NULL,
    fallthrough REAL NOT NULL CHECK (fallthrough >= 0 AND fallthrough <= 1),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    UNIQUE (flag_id, environment_id)
);

-- The unique constraint already indexes lookups by flag; this one covers
-- lookups by environment and the cascade when an environment is deleted.
CREATE INDEX flag_settings_environment_id_idx ON flag_settings (environment_id);

-- +goose Down
DROP TABLE flag_settings;
