-- name: CreateChangeLogsForFlag :exec
-- Logs a new flag in every environment, with the setting it starts with there
-- and the environment's version. It reads both, so it runs after the settings
-- are created and the versions bumped. Nothing is renamed, so the old key is
-- left empty. The clock is read once so all the entries for one flag
-- share a created_at.
WITH now AS (SELECT clock_timestamp() AS ts)
INSERT INTO change_logs (
  id,
  target_version,
  environment_id,
  flag_new_key,
  enabled,
  fallthrough,
  action,
  created_at
)
SELECT
  gen_random_uuid(),
  e.version,
  e.id,
  f.key,
  fs.enabled,
  fs.fallthrough,
  'flag-created',
  now.ts
FROM flag_settings fs
JOIN environments e ON e.id = fs.environment_id
JOIN flags f ON f.id = fs.flag_id
CROSS JOIN now
WHERE fs.flag_id = sqlc.arg('flag_id');

-- name: CreateChangeLogsForFlagUpdate :exec
-- Logs a flag's new key in every environment, next to the key it replaced,
-- with the environment's version. It reads the new key and the versions, so it
-- runs after the flag is updated and the versions bumped. The clock is read
-- once so all the entries for one update share a created_at.
WITH now AS (SELECT clock_timestamp() AS ts)
INSERT INTO change_logs (
  id,
  target_version,
  environment_id,
  flag_old_key,
  flag_new_key,
  action,
  created_at
)
SELECT
  gen_random_uuid(),
  e.version,
  e.id,
  sqlc.arg('old_key')::varchar,
  f.key,
  'flag-updated',
  now.ts
FROM environments e
CROSS JOIN flags f
CROSS JOIN now
WHERE f.id = sqlc.arg('flag_id');

-- name: CreateChangeLogsForFlagDelete :exec
-- Logs a flag's deletion in every environment, with the flag's key and the
-- environment's version. The flag is gone by then, so its key is passed in;
-- the versions are read, so it runs after they're bumped. The clock is read
-- once so all the entries for one deletion share a created_at.
WITH now AS (SELECT clock_timestamp() As ts)
INSERT INTO change_logs (
  id,
  target_version,
  environment_id,
  flag_new_key,
  action,
  created_at
)
SELECT
  gen_random_uuid(),
  e.version,
  e.id,
  sqlc.arg('flag_key')::varchar,
  'flag-deleted',
  now.ts
FROM environments e
CROSS JOIN now;
