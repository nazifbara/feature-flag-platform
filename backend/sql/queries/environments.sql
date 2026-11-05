-- name: CreateEnv :one
WITH now AS (SELECT clock_timestamp() as ts)
INSERT INTO environments (
  id,
  name,
  created_at,
  updated_at
)
SELECT gen_random_uuid(), $1, ts, ts FROM now
RETURNING *;

-- name: GetEnv :one
SELECT * FROM environments WHERE id = $1;

-- name: ListEnvs :many
SELECT * FROM environments ORDER BY created_at ASC;

-- name: UpdateEnv :one
UPDATE environments SET name = $2, updated_at = clock_timestamp() WHERE id = $1
RETURNING *;

-- name: DeleteEnv :one
DELETE FROM environments WHERE id = $1
RETURNING *;

-- name: IncrementAllEnvVersions :exec
UPDATE environments SET version = version + 1, updated_at = clock_timestamp();

-- name: IncrementEnvVersion :exec
UPDATE environments SET version = version + 1, updated_at = clock_timestamp() WHERE id = $1;
