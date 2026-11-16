-- +goose Up
-- The env-deleted action becomes environment-deleted, like the API's
-- EnvironmentService. Entries already logged are renamed with it, so SDKs
-- syncing past them see one name.
ALTER TABLE change_logs
DROP CONSTRAINT change_logs_action_check,
DROP CONSTRAINT env_deleted_fields;

UPDATE change_logs SET action = 'environment-deleted' WHERE action = 'env-deleted';

ALTER TABLE change_logs
ADD CONSTRAINT change_logs_action_check CHECK (
    action IN (
        'environment-deleted',
        'flag-created', 'flag-updated', 'flag-deleted',
        'flag-archived', 'flag-unarchived',
        'setting-updated',
        'rule-created', 'rule-updated', 'rule-deleted', 'rule-position-changed'
    )
),
ADD CONSTRAINT environment_deleted_fields CHECK (
    action <> 'environment-deleted' OR num_nonnulls(
        flag_old_key, flag_new_key, enabled, fallthrough,
        rule_serve, rule_clauses, rule_old_position, rule_new_position
    ) = 0
);

-- +goose Down
-- Renaming the entries back loses nothing: the two names mean the same.
ALTER TABLE change_logs
DROP CONSTRAINT change_logs_action_check,
DROP CONSTRAINT environment_deleted_fields;

UPDATE change_logs SET action = 'env-deleted' WHERE action = 'environment-deleted';

ALTER TABLE change_logs
ADD CONSTRAINT change_logs_action_check CHECK (
    action IN (
        'env-deleted',
        'flag-created', 'flag-updated', 'flag-deleted',
        'flag-archived', 'flag-unarchived',
        'setting-updated',
        'rule-created', 'rule-updated', 'rule-deleted', 'rule-position-changed'
    )
),
ADD CONSTRAINT env_deleted_fields CHECK (
    action <> 'env-deleted' OR num_nonnulls(
        flag_old_key, flag_new_key, enabled, fallthrough,
        rule_serve, rule_clauses, rule_old_position, rule_new_position
    ) = 0
);
