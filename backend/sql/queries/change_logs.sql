-- name: CreateChangeLogsForEnvDelete :exec
-- Logs an environment's deletion, with the version after the one it was at, so
-- SDKs syncing it see a newer version. The environment is gone by then, so its
-- id and the version are passed in.
INSERT INTO change_logs (
    id,
    target_version,
    environment_id,
    action,
    created_at
) VALUES (
    gen_random_uuid(),
    sqlc.arg('version'),
    sqlc.arg('environment_id'),
    'env-deleted',
    clock_timestamp()
);

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
FROM flag_settings AS fs
INNER JOIN environments AS e ON fs.environment_id = e.id
INNER JOIN flags AS f ON fs.flag_id = f.id
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
FROM environments AS e
CROSS JOIN flags AS f
CROSS JOIN now
WHERE f.id = sqlc.arg('flag_id');

-- name: CreateChangeLogsForFlagDelete :exec
-- Logs a flag's deletion in every environment, with the flag's key and the
-- environment's version. The flag is gone by then, so its key is passed in;
-- the versions are read, so it runs after they're bumped. The clock is read
-- once so all the entries for one deletion share a created_at.
WITH now AS (SELECT clock_timestamp() AS ts)

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
FROM environments AS e
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
FROM flag_settings AS fs
CROSS JOIN flags AS f
CROSS JOIN environments AS e
CROSS JOIN now
WHERE
    e.id = fs.environment_id
    AND f.id = fs.flag_id
    AND fs.id = sqlc.arg('flag_setting_id');

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
FROM rules AS r
CROSS JOIN environments AS e
CROSS JOIN flags AS f
CROSS JOIN flag_settings AS fs
CROSS JOIN now
WHERE
    r.id = sqlc.arg('rule_id')
    AND fs.id = r.flag_setting_id
    AND f.id = fs.flag_id
    AND e.id = fs.environment_id;

-- name: CreateChangeLogsForRuleUpdate :exec
-- Logs a rule's new serve and clauses in its setting's environment, with the
-- flag's key, the rule's position and the environment's version. It reads them
-- all, so it runs after the rule is updated and the version bumped.
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
    'rule-updated',
    now.ts
FROM rules AS r
CROSS JOIN environments AS e
CROSS JOIN flags AS f
CROSS JOIN flag_settings AS fs
CROSS JOIN now
WHERE
    r.id = sqlc.arg('rule_id')
    AND fs.id = r.flag_setting_id
    AND f.id = fs.flag_id
    AND e.id = fs.environment_id;


-- name: CreateChangeLogsForRulePositionChange :exec
-- Logs a rule's move in its setting's environment, with the flag's key, the
-- position it left and the one it took, and the environment's version. The old
-- position is gone by then, so it's passed in; the new one and the version are
-- read, so it runs after the rule is moved and the version bumped.
WITH now AS (SELECT clock_timestamp() AS ts)

INSERT INTO change_logs (
    id,
    target_version,
    environment_id,
    flag_new_key,
    rule_old_position,
    rule_new_position,
    action,
    created_at
)
SELECT
    gen_random_uuid(),
    e.version,
    e.id,
    f.key,
    sqlc.arg('old_position')::integer,
    r.position,
    'rule-position-changed',
    now.ts
FROM rules AS r
CROSS JOIN environments AS e
CROSS JOIN flags AS f
CROSS JOIN flag_settings AS fs
CROSS JOIN now
WHERE
    r.id = sqlc.arg('rule_id')
    AND fs.id = r.flag_setting_id
    AND f.id = fs.flag_id
    AND e.id = fs.environment_id;


-- name: CreateChangeLogsForRuleDelete :exec
-- Logs a rule's deletion in its setting's environment, with the flag's key, the
-- position the rule had and the environment's version. The rule is gone by then,
-- so its setting and position are passed in; the version is read, so it runs
-- after it's bumped.
WITH now AS (SELECT clock_timestamp() AS ts)

INSERT INTO change_logs (
    id,
    target_version,
    environment_id,
    flag_new_key,
    rule_new_position,
    action,
    created_at
)
SELECT
    gen_random_uuid(),
    e.version,
    e.id,
    f.key,
    sqlc.arg('position')::integer,
    'rule-deleted',
    now.ts
FROM flag_settings AS fs
CROSS JOIN environments AS e
CROSS JOIN flags AS f
CROSS JOIN now
WHERE
    fs.id = sqlc.arg('flag_setting_id')
    AND f.id = fs.flag_id
    AND e.id = fs.environment_id;
