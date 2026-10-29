-- +goose Up
CREATE TABLE environments (
  id UUID PRIMARY KEY,
  name VARCHAR(100) NOT NULL UNIQUE
);

-- +goose Down
DROP TABLE environments;
