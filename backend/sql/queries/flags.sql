-- name: CreateFlag :one
-- The clock is read once so a new flag's created_at and updated_at are equal.
WITH now AS (SELECT clock_timestamp() AS ts)
INSERT INTO flags (
  id,
  key,
  created_at,
  updated_at
)
SELECT gen_random_uuid(), $1, ts, ts FROM now
RETURNING *;
