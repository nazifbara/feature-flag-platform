-- +goose Up
CREATE TABLE environments (
  id UUID PRIMARY KEY,
  name VARCHAR(100) NOT NULL UNIQUE,
  created_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE environments;
