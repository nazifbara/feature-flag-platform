-- name: CreateFlag :one
-- The clock is read once so a new flag's created_at and updated_at are equal.
WITH now AS (SELECT clock_timestamp() AS ts)

INSERT INTO flags (
    id,
    key,
    salt,
    created_at,
    updated_at
)
SELECT
    gen_random_uuid(),
    $1,
    $2,
    ts,
    ts
FROM now
RETURNING *;

-- name: GetFlag :one
SELECT * FROM flags
WHERE id = $1;

-- name: UpdateFlag :one
UPDATE flags SET updated_at = clock_timestamp(), key = $2
WHERE id = $1
RETURNING *;

-- name: DeleteFlag :one
DELETE FROM flags
WHERE id = $1
RETURNING *;

-- name: ListFlags :many
-- Ordered by key in the database's collation, starting after key, or at the
-- first flag for a NULL key, and stopping at limit flags.
SELECT * FROM flags
WHERE
    sqlc.narg('key')::varchar IS NULL
    OR key > sqlc.narg('key')::varchar
ORDER BY key ASC
LIMIT sqlc.arg('limit');
