-- name: GetFlagSetting :one
SELECT * FROM flag_settings WHERE flag_id = $1 AND environment_id = $2;

-- name: UpdateFlagSetting :one
UPDATE flag_settings
SET enabled = $3, fallthrough = $4, updated_at = clock_timestamp()
WHERE flag_id = $1 AND environment_id = $2
RETURNING *;

-- name: ListSettingsByFlag :many
SELECT fs.* FROM flag_settings fs
JOIN environments e ON e.id = fs.environment_id
WHERE fs.flag_id = $1
ORDER BY e.created_at ASC, e.id ASC;

-- name: ListSettingsByEnv :many
SELECT sqlc.embed(fs), f.key
FROM flag_settings fs
JOIN flags f ON f.id = fs.flag_id
WHERE fs.environment_id = $1
ORDER BY f.key;

-- name: LockFlagSetting :one
SELECT id FROM flag_settings WHERE id = $1 FOR UPDATE;

-- name: LockFlagEnvMatrix :exec
-- Serializes CreateFlag and CreateEnv so neither can miss the other's new row.
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
SELECT gen_random_uuid(), $1, e.id, false, 0, now.ts, now.ts
FROM environments e CROSS JOIN now
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
SELECT gen_random_uuid(), f.id, $1, false, 0, now.ts, now.ts
FROM flags f CROSS JOIN now
FOR KEY SHARE OF f;
