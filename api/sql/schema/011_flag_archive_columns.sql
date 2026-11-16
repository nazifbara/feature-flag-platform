-- +goose Up
ALTER TABLE flags
ADD COLUMN archived BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE flags
DROP COLUMN archived;
