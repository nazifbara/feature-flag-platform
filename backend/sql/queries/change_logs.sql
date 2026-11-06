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
