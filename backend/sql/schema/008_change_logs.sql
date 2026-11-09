-- +goose Up
CREATE TABLE change_logs (
    id UUID PRIMARY KEY,
    target_version BIGINT NOT NULL,
    environment_id UUID NOT NULL,
    flag_old_key VARCHAR(100),
    flag_new_key VARCHAR(100),
    enabled BOOLEAN,
    fallthrough REAL CHECK (fallthrough >= 0 AND fallthrough <= 1),
    rule_serve REAL CHECK (rule_serve >= 0 AND rule_serve <= 1),
    rule_clauses JSONB CHECK (jsonb_typeof(rule_clauses) = 'array' AND rule_clauses <> '[]'::JSONB),
    rule_old_position INTEGER CHECK (rule_old_position >= 0),
    rule_new_position INTEGER CHECK (rule_new_position >= 0),
    action VARCHAR(30) NOT NULL CHECK (
        action IN (
            'env-deleted',
            'flag-created', 'flag-updated', 'flag-deleted',
            'setting-updated',
            'rule-created', 'rule-updated', 'rule-deleted', 'rule-position-changed'
        )
    ),
    created_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT change_logs_env_version_unique UNIQUE (environment_id, target_version),

    -- Each action requires the columns it describes and forbids the rest. The
    -- new key or position says what a row is about; the old one is only kept by
    -- the action that changes it.
    CONSTRAINT env_deleted_fields CHECK (
        action <> 'env-deleted' OR num_nonnulls(
            flag_old_key, flag_new_key, enabled, fallthrough,
            rule_serve, rule_clauses, rule_old_position, rule_new_position
        ) = 0
    ),
    CONSTRAINT flag_created_fields CHECK (
        action <> 'flag-created' OR (
            num_nulls(flag_new_key, enabled, fallthrough) = 0
            AND num_nonnulls(
                flag_old_key, rule_serve, rule_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT flag_updated_fields CHECK (
        action <> 'flag-updated' OR (
            num_nulls(flag_old_key, flag_new_key) = 0
            AND num_nonnulls(
                enabled, fallthrough,
                rule_serve, rule_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT flag_deleted_fields CHECK (
        action <> 'flag-deleted' OR (
            flag_new_key IS NOT NULL
            AND num_nonnulls(
                flag_old_key, enabled, fallthrough,
                rule_serve, rule_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT setting_updated_fields CHECK (
        action <> 'setting-updated' OR (
            num_nulls(flag_new_key, enabled, fallthrough) = 0
            AND num_nonnulls(
                flag_old_key, rule_serve, rule_clauses, rule_old_position, rule_new_position
            ) = 0
        )
    ),
    CONSTRAINT rule_created_fields CHECK (
        action <> 'rule-created' OR (
            num_nulls(flag_new_key, rule_serve, rule_clauses, rule_new_position) = 0
            AND num_nonnulls(flag_old_key, enabled, fallthrough, rule_old_position) = 0
        )
    ),
    CONSTRAINT rule_updated_fields CHECK (
        action <> 'rule-updated' OR (
            num_nulls(flag_new_key, rule_serve, rule_clauses, rule_new_position) = 0
            AND num_nonnulls(flag_old_key, enabled, fallthrough, rule_old_position) = 0
        )
    ),
    CONSTRAINT rule_deleted_fields CHECK (
        action <> 'rule-deleted' OR (
            num_nulls(flag_new_key, rule_new_position) = 0
            AND num_nonnulls(
                flag_old_key, enabled, fallthrough,
                rule_serve, rule_clauses, rule_old_position
            ) = 0
        )
    ),
    CONSTRAINT rule_position_changed_fields CHECK (
        action <> 'rule-position-changed' OR (
            num_nulls(flag_new_key, rule_old_position, rule_new_position) = 0
            AND num_nonnulls(flag_old_key, enabled, fallthrough, rule_serve, rule_clauses) = 0
        )
    )
);

-- +goose Down
DROP TABLE change_logs;
