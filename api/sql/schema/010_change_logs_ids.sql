-- +goose Up
-- Evaluating a flag takes its id and salt, which bucket users for rollouts, and
-- each rule's id, so an SDK catching up from the log can only evaluate a flag
-- or rule created since its snapshot if the entry creating it carries them.
-- Only those entries do. NOT VALID leaves entries logged before this alone.
ALTER TABLE change_logs
ADD COLUMN flag_id UUID,
ADD COLUMN flag_salt VARCHAR(100),
ADD COLUMN rule_id UUID,
ADD CONSTRAINT flag_identity_fields CHECK (
    (action = 'flag-created') = (flag_id IS NOT NULL)
    AND (action = 'flag-created') = (flag_salt IS NOT NULL)
) NOT VALID,
ADD CONSTRAINT rule_identity_fields CHECK (
    (action = 'rule-created') = (rule_id IS NOT NULL)
) NOT VALID;

-- +goose Down
ALTER TABLE change_logs
DROP COLUMN rule_id,
DROP COLUMN flag_salt,
DROP COLUMN flag_id;
