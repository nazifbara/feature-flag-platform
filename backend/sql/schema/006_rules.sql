-- +goose Up
CREATE TABLE rules (
  id UUID PRIMARY KEY,
  flag_setting_id UUID NOT NULL REFERENCES flag_settings (id) ON DELETE CASCADE,
  position INTEGER NOT NULL,
  name VARCHAR(200),
  serve REAL NOT NULL CHECK (serve >= 0 AND serve <= 1),
  clauses JSONB NOT NULL CHECK (jsonb_typeof(clauses) = 'array' AND clauses <> '[]'::jsonb),
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  -- Deferred so two rules can swap positions without a conflict.
  UNIQUE (flag_setting_id, position) DEFERRABLE INITIALLY DEFERRED
);

-- +goose Down
DROP TABLE rules;
