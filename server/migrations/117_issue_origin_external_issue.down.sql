-- Revert to the pre-external-import origin type list.
DROP INDEX IF EXISTS idx_issue_external_origin_unique;

ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat'));
