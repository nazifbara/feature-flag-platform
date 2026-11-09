-- +goose Up
CREATE TABLE rules (
    id UUID PRIMARY KEY,
    flag_setting_id UUID NOT NULL
    CONSTRAINT rules_setting_fk REFERENCES flag_settings (id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    name VARCHAR(200) NOT NULL,
    serve REAL NOT NULL CHECK (serve >= 0 AND serve <= 1),
    clauses JSONB NOT NULL CHECK (jsonb_typeof(clauses) = 'array' AND clauses <> '[]'::JSONB),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    -- Deferred so two rules can swap positions without a conflict.
    CONSTRAINT rules_setting_position_unique UNIQUE (flag_setting_id, position)
    DEFERRABLE INITIALLY DEFERRED
);

-- +goose Down
DROP TABLE rules;
