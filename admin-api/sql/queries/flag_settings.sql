-- name: GetFlagSetting :one
SELECT * FROM flag_settings
WHERE flag_id = $1 AND environment_id = $2;

-- name: GetFlagSettingByID :one
-- Gets one setting by its own id, as the admin API names settings.
SELECT * FROM flag_settings
WHERE id = $1;

-- name: UpdateFlagSetting :one
-- Replaces the setting's values; the store checks them and logs the change.
UPDATE flag_settings
SET enabled = $2, fallthrough = $3, updated_at = clock_timestamp()
WHERE id = $1
RETURNING *;

-- name: ListSettingsByFlag :many
SELECT fs.* FROM flag_settings AS fs
INNER JOIN environments AS e ON fs.environment_id = e.id
WHERE fs.flag_id = $1
ORDER BY e.created_at ASC, e.id ASC;

-- name: ListSettingsByEnv :many
SELECT
    f.key,
    sqlc.embed(fs)
FROM flag_settings AS fs
INNER JOIN flags AS f ON fs.flag_id = f.id
WHERE fs.environment_id = $1
ORDER BY f.key;

-- name: LockFlagEnvMatrix :exec
-- Taken first by writes to the flag/env matrix, so CreateFlag and CreateEnv can't
-- miss each other's new row and writers can't deadlock on the rows they share.
SELECT pg_advisory_xact_lock(7236401);

-- name: CreateSettingsForFlag :exec
WITH now AS (SELECT clock_timestamp() AS ts)

INSERT INTO flag_settings (
    id,
    flag_id,
    environment_id,
    enabled,
    fallthrough,
    created_at,
    updated_at
)
SELECT
    gen_random_uuid(),
    $1,
    e.id,
    false,
    0,
    now.ts,
    now.ts
FROM environments AS e CROSS JOIN now
FOR KEY SHARE OF e;

-- name: CreateSettingsForEnv :exec
WITH now AS (SELECT clock_timestamp() AS ts)

INSERT INTO flag_settings (
    id,
    flag_id,
    environment_id,
    enabled,
    fallthrough,
    created_at,
    updated_at
)
SELECT
    gen_random_uuid(),
    f.id,
    $1,
    false,
    0,
    now.ts,
    now.ts
FROM flags AS f CROSS JOIN now
FOR KEY SHARE OF f;
