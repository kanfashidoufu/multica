-- This repair migration intentionally keeps the corrected invariant on rollback.
-- Reintroducing the released v0.3.34 constraint would reject valid
-- origin_type='external_issue' rows and can break external issue imports.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'external_issue', 'slack_chat'));
