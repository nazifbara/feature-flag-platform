-- +goose Up
CREATE TABLE flags (
  id UUID PRIMARY KEY,
  key VARCHAR(100) NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE flags;
