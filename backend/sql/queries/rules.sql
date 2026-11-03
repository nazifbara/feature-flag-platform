-- name: CreateRule :one
WITH now AS (SELECT clock_timestamp() AS ts)
INSERT INTO rules (
  id,
  flag_setting_id,
  position,
  name,
  serve,
  clauses,
  created_at,
  updated_at
)
SELECT gen_random_uuid(), $1, $2, $3, $4, $5, ts, ts FROM now
RETURNING *;
