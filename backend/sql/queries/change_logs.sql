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

-- name: CreateChangeLogsForSettingUpdate :exec
-- Logs a flag's new setting in one environment, with the flag's key and the
-- environment's version. It reads both, so it runs after the setting is
-- updated and the version bumped.
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
  fs.environment_id,
  f.key,
  fs.enabled,
  fs.fallthrough,
  'setting-updated',
  now.ts
FROM flag_settings fs
CROSS JOIN flags f
CROSS JOIN environments e
CROSS JOIN now
WHERE
  e.id = fs.environment_id AND
  f.id = fs.flag_id AND
  fs.id = sqlc.arg('flag_setting_id');

-- name: CreateChangeLogsForRuleCreate :exec
-- Logs a new rule in its setting's environment, with the flag's key, the rule's
-- serve, clauses and position, and the environment's version. It reads them
-- all, so it runs after the rule is created and the version bumped.
WITH now AS (SELECT clock_timestamp() AS ts)
INSERT INTO change_logs (
  id,
  target_version,
  environment_id,
  flag_new_key,
  rule_serve,
  rule_clauses,
  rule_new_position,
  action,
  created_at
)
SELECT
  gen_random_uuid(),
  e.version,
  e.id,
  f.key,
  r.serve,
  r.clauses,
  r.position,
  'rule-created',
  now.ts
FROM rules r
CROSS JOIN environments e
CROSS JOIN flags f
CROSS JOIN flag_settings fs
CROSS JOIN now
WHERE
  r.id = sqlc.arg('rule_id') AND
  fs.id = r.flag_setting_id AND
  f.id = fs.flag_id AND
  e.id = fs.environment_id;
