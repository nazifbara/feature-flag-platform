-- +goose Up
-- Records who changed what, for admins reading the history. Unlike change_logs,
-- which SDKs sync from, entries aren't versioned and keep the values a write
-- replaced as well as the ones it set: old columns hold the values before the
-- write and new ones the values after, each set only by the actions that have
-- that side. Creating and renaming an environment and creating a setting are
-- logged here only. A flag's own actions are logged once, not per environment
-- as in change_logs, since a flag's key, state and existence aren't any one
-- environment's.
CREATE TABLE audit_logs (
    id UUID PRIMARY KEY,
    -- No foreign keys: entries outlive their actor and what they name, and
    -- each actor type keeps its actors in its own table.
    actor_type VARCHAR(30) NOT NULL CHECK (actor_type IN ('user')),
    actor_id UUID NOT NULL,
    -- The call that made the change, as logged and sent back in X-Request-Id.
    -- It groups the entries one call writes, such as a flag's creation and the
    -- settings created with it. Clients may choose it, so it isn't unique
    -- across calls.
    request_id UUID NOT NULL,
    -- The environment the entry is about, set by every action but a flag's own.
    environment_id UUID,
    -- The environment's name when the action happened, so entries still say
    -- which one they're about once it's renamed or deleted. A rename leaves the
    -- new name here and the old one in environment_old_name.
    environment_name VARCHAR(100),
    environment_old_name VARCHAR(100),
    flag_id UUID,
    -- The flag's key when the action happened. A rename leaves the new key
    -- here and the old one in flag_old_key.
    flag_key VARCHAR(100),
    flag_old_key VARCHAR(100),
    rule_id UUID,
    setting_old_enabled BOOLEAN,
    setting_new_enabled BOOLEAN,
    setting_old_fallthrough REAL CHECK (setting_old_fallthrough >= 0 AND setting_old_fallthrough <= 1),
    setting_new_fallthrough REAL CHECK (setting_new_fallthrough >= 0 AND setting_new_fallthrough <= 1),
    rule_old_name VARCHAR(200),
    rule_new_name VARCHAR(200),
    rule_old_serve REAL CHECK (rule_old_serve >= 0 AND rule_old_serve <= 1),
    rule_new_serve REAL CHECK (rule_new_serve >= 0 AND rule_new_serve <= 1),
    rule_old_clauses JSONB CHECK (
        jsonb_typeof(rule_old_clauses) = 'array' AND rule_old_clauses <> '[]'::JSONB
    ),
    rule_new_clauses JSONB CHECK (
        jsonb_typeof(rule_new_clauses) = 'array' AND rule_new_clauses <> '[]'::JSONB
    ),
    rule_old_position INTEGER CHECK (rule_old_position >= 0),
    rule_new_position INTEGER CHECK (rule_new_position >= 0),
    action VARCHAR(30) NOT NULL CHECK (
        action IN (
            'environment-created', 'environment-renamed', 'environment-deleted',
            'flag-created', 'flag-updated', 'flag-deleted',
            'flag-archived', 'flag-unarchived',
            'setting-created', 'setting-updated',
            'rule-created', 'rule-updated', 'rule-deleted', 'rule-position-changed'
        )
    ),
    created_at TIMESTAMPTZ NOT NULL,

    -- Each action requires the columns it describes and forbids the rest.
    -- Creating and deleting an environment only name it.
    CONSTRAINT environment_named_fields CHECK (
        action NOT IN ('environment-created', 'environment-deleted') OR (
            num_nulls(environment_id, environment_name) = 0
            AND num_nonnulls(
                environment_old_name, flag_id, flag_key, flag_old_key, rule_id,
                setting_old_enabled, setting_new_enabled,
                setting_old_fallthrough, setting_new_fallthrough,
                rule_old_name, rule_new_name, rule_old_serve, rule_new_serve,
                rule_old_clauses, rule_new_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT environment_renamed_fields CHECK (
        action <> 'environment-renamed' OR (
            num_nulls(environment_id, environment_name, environment_old_name) = 0
            AND num_nonnulls(
                flag_id, flag_key, flag_old_key, rule_id,
                setting_old_enabled, setting_new_enabled,
                setting_old_fallthrough, setting_new_fallthrough,
                rule_old_name, rule_new_name, rule_old_serve, rule_new_serve,
                rule_old_clauses, rule_new_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    -- Creating, deleting, archiving and unarchiving a flag only name it. The
    -- settings created with a flag have setting-created entries of their own.
    CONSTRAINT flag_named_fields CHECK (
        action NOT IN ('flag-created', 'flag-deleted', 'flag-archived', 'flag-unarchived') OR (
            num_nulls(flag_id, flag_key) = 0
            AND num_nonnulls(
                environment_id, environment_name, environment_old_name, flag_old_key, rule_id,
                setting_old_enabled, setting_new_enabled,
                setting_old_fallthrough, setting_new_fallthrough,
                rule_old_name, rule_new_name, rule_old_serve, rule_new_serve,
                rule_old_clauses, rule_new_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT flag_updated_fields CHECK (
        action <> 'flag-updated' OR (
            num_nulls(flag_id, flag_key, flag_old_key) = 0
            AND num_nonnulls(
                environment_id, environment_name, environment_old_name, rule_id,
                setting_old_enabled, setting_new_enabled,
                setting_old_fallthrough, setting_new_fallthrough,
                rule_old_name, rule_new_name, rule_old_serve, rule_new_serve,
                rule_old_clauses, rule_new_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT setting_created_fields CHECK (
        action <> 'setting-created' OR (
            num_nulls(
                environment_id, environment_name, flag_id, flag_key,
                setting_new_enabled, setting_new_fallthrough
            ) = 0
            AND num_nonnulls(
                environment_old_name, flag_old_key, rule_id,
                setting_old_enabled, setting_old_fallthrough,
                rule_old_name, rule_new_name, rule_old_serve, rule_new_serve,
                rule_old_clauses, rule_new_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT setting_updated_fields CHECK (
        action <> 'setting-updated' OR (
            num_nulls(
                environment_id, environment_name, flag_id, flag_key,
                setting_old_enabled, setting_new_enabled,
                setting_old_fallthrough, setting_new_fallthrough
            ) = 0
            AND num_nonnulls(
                environment_old_name, flag_old_key, rule_id,
                rule_old_name, rule_new_name, rule_old_serve, rule_new_serve,
                rule_old_clauses, rule_new_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT rule_created_fields CHECK (
        action <> 'rule-created' OR (
            num_nulls(
                environment_id, environment_name, flag_id, flag_key, rule_id,
                rule_new_name, rule_new_serve, rule_new_clauses, rule_new_position
            ) = 0
            AND num_nonnulls(
                environment_old_name, flag_old_key,
                setting_old_enabled, setting_new_enabled,
                setting_old_fallthrough, setting_new_fallthrough,
                rule_old_name, rule_old_serve, rule_old_clauses, rule_old_position
            ) = 0
        )
    ),
    -- An update can't move a rule, so its position is left out.
    CONSTRAINT rule_updated_fields CHECK (
        action <> 'rule-updated' OR (
            num_nulls(
                environment_id, environment_name, flag_id, flag_key, rule_id,
                rule_old_name, rule_new_name, rule_old_serve, rule_new_serve,
                rule_old_clauses, rule_new_clauses
            ) = 0
            AND num_nonnulls(
                environment_old_name, flag_old_key,
                setting_old_enabled, setting_new_enabled,
                setting_old_fallthrough, setting_new_fallthrough,
                rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT rule_deleted_fields CHECK (
        action <> 'rule-deleted' OR (
            num_nulls(
                environment_id, environment_name, flag_id, flag_key, rule_id,
                rule_old_name, rule_old_serve, rule_old_clauses, rule_old_position
            ) = 0
            AND num_nonnulls(
                environment_old_name, flag_old_key,
                setting_old_enabled, setting_new_enabled,
                setting_old_fallthrough, setting_new_fallthrough,
                rule_new_name, rule_new_serve, rule_new_clauses, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT rule_position_changed_fields CHECK (
        action <> 'rule-position-changed' OR (
            num_nulls(
                environment_id, environment_name, flag_id, flag_key, rule_id,
                rule_old_position, rule_new_position
            ) = 0
            AND num_nonnulls(
                environment_old_name, flag_old_key,
                setting_old_enabled, setting_new_enabled,
                setting_old_fallthrough, setting_new_fallthrough,
                rule_old_name, rule_new_name, rule_old_serve, rule_new_serve,
                rule_old_clauses, rule_new_clauses
            ) = 0
        )
    )
);

-- +goose Down
DROP TABLE audit_logs;
