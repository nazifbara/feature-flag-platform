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
SELECT
    gen_random_uuid(),
    $1,
    $2,
    $3,
    $4,
    $5,
    ts,
    ts
FROM now
RETURNING *;

-- name: GetRule :one
SELECT * FROM rules
WHERE id = $1;

-- name: ListRulesBySetting :many
SELECT * FROM rules
WHERE flag_setting_id = $1
ORDER BY position;

-- name: DeleteRule :one
DELETE FROM rules
WHERE id = $1 RETURNING *;

-- name: UpdateRule :one
UPDATE rules
SET
    name = $1,
    serve = $2,
    clauses = $3,
    updated_at = clock_timestamp()
WHERE id = $4
RETURNING *;

-- name: CountRulesBySetting :one
SELECT count(*) FROM rules
WHERE flag_setting_id = $1;

-- name: ShiftRulePositionsBetween :exec
UPDATE rules
SET
    position = position + sqlc.arg(delta),
    updated_at = clock_timestamp()
WHERE
    position > sqlc.arg(after)
    AND position < sqlc.arg(before)::bigint
    AND flag_setting_id = sqlc.arg(flag_setting_id);

-- name: ChangeRulePosition :one
UPDATE rules
SET
    position = $1,
    updated_at = clock_timestamp()
WHERE id = $2
RETURNING *;
