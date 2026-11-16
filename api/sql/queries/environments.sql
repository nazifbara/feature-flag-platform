-- name: CreateEnv :one
WITH now AS (SELECT clock_timestamp() AS ts)

INSERT INTO environments (
    id,
    name,
    created_at,
    updated_at
)
SELECT
    gen_random_uuid(),
    $1,
    ts,
    ts
FROM now
RETURNING *;

-- name: GetEnv :one
SELECT * FROM environments
WHERE id = $1;

-- name: ListEnvs :many
-- Oldest first, ordered by created_at then id, starting after the environment
-- created at created_at with id, or at the first one for a NULL created_at, and
-- stopping at limit environments. The id breaks ties between environments
-- created at the same instant.
SELECT * FROM environments
WHERE
    sqlc.narg('created_at')::timestamptz IS NULL
    OR (created_at, id) > (sqlc.narg('created_at')::timestamptz, sqlc.narg('id')::uuid)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg('limit');

-- name: UpdateEnv :one
UPDATE environments SET name = $2, updated_at = clock_timestamp()
WHERE id = $1
RETURNING *;

-- name: DeleteEnv :one
DELETE FROM environments
WHERE id = $1
RETURNING *;

-- name: IncrementAllEnvVersions :exec
UPDATE environments SET version = version + 1, updated_at = clock_timestamp();

-- name: IncrementEnvVersion :exec
UPDATE environments SET version = version + 1, updated_at = clock_timestamp()
WHERE id = $1;
