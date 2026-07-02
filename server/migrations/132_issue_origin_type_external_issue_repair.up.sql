-- Repair the v0.3.34 131_issue_origin_slack_chat migration, which added
-- slack_chat but accidentally dropped external_issue from the CHECK list.
-- Databases that had already applied the released 131 need a later migration
-- to rebuild the constraint with the full origin set.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'external_issue', 'slack_chat'));
