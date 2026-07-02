-- Revert to the pre-slack_chat issue_origin_type_check list. Any existing rows
-- with origin_type='slack_chat' would violate the rolled-back constraint; the
-- down migration assumes the operator has already deleted or relabeled those
-- rows. Keep external_issue from 117, since it existed before this migration.
-- Kept strict (no DROP NOT VALID dance) to preserve the schema invariant
-- downstream code relies on. Mirrors 111.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'external_issue'));
