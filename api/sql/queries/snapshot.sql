-- name: SnapshotEnv :one
-- Gives an environment as an SDK syncs it from scratch: its version and name,
-- and each flag's setting there by flag key: the flag's id and salt, which
-- bucket users for rollouts, the switch, the fallthrough and the rules in
-- position order. It's one statement, so the version and the settings come
-- from the same moment.
WITH settings AS (
    SELECT
        fs.environment_id,
        jsonb_object_agg(
            f.key,
            jsonb_build_object(
                'id', f.id,
                'salt', f.salt,
                'enabled', fs.enabled,
                'fallthrough', fs.fallthrough,
                'rules', coalesce((
                    SELECT
                        jsonb_agg(
                            jsonb_build_object(
                                'id', r.id,
                                'serve', r.serve,
                                'stored_clauses', r.clauses
                            )
                            ORDER BY r.position
                        )
                    FROM rules AS r
                    WHERE r.flag_setting_id = fs.id
                ), '[]'::jsonb)
            )
        ) AS data
    FROM flag_settings AS fs
    INNER JOIN flags AS f ON fs.flag_id = f.id
    GROUP BY fs.environment_id
)

SELECT
    jsonb_build_object(
        'version', e.version,
        'name', e.name,
        'flag_settings', settings.data
    ) AS result
FROM environments AS e
LEFT JOIN settings ON e.id = settings.environment_id
WHERE e.id = $1;
