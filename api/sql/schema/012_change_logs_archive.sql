-- +goose Up
-- Archiving and unarchiving a flag is logged in every environment, like its
-- deletion: the entry names the flag by its key and carries nothing else.
ALTER TABLE change_logs
DROP CONSTRAINT change_logs_action_check,
ADD CONSTRAINT change_logs_action_check CHECK (
    action IN (
        'env-deleted',
        'flag-created', 'flag-updated', 'flag-deleted',
        'flag-archived', 'flag-unarchived',
        'setting-updated',
        'rule-created', 'rule-updated', 'rule-deleted', 'rule-position-changed'
    )
),
ADD CONSTRAINT flag_archived_fields CHECK (
    action <> 'flag-archived' OR (
        flag_new_key IS NOT NULL
        AND num_nonnulls(
            flag_old_key, enabled, fallthrough,
            rule_serve, rule_clauses, rule_old_position, rule_new_position
        ) = 0
    )
),
ADD CONSTRAINT flag_unarchived_fields CHECK (
    action <> 'flag-unarchived' OR (
        flag_new_key IS NOT NULL
        AND num_nonnulls(
            flag_old_key, enabled, fallthrough,
            rule_serve, rule_clauses, rule_old_position, rule_new_position
        ) = 0
    )
);

-- +goose Down
-- NOT VALID keeps entries logged by archiving and unarchiving: deleting them
-- would leave gaps in their environments' versions, which SDKs sync by.
ALTER TABLE change_logs
DROP CONSTRAINT flag_unarchived_fields,
DROP CONSTRAINT flag_archived_fields,
DROP CONSTRAINT change_logs_action_check,
ADD CONSTRAINT change_logs_action_check CHECK (
    action IN (
        'env-deleted',
        'flag-created', 'flag-updated', 'flag-deleted',
        'setting-updated',
        'rule-created', 'rule-updated', 'rule-deleted', 'rule-position-changed'
    )
) NOT VALID;
