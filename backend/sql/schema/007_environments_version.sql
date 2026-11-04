-- +goose Up
ALTER TABLE environments ADD COLUMN version BIGINT NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE environments DROP COLUMN version;
