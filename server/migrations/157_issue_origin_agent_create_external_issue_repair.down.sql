-- Keep the repaired invariant on rollback. Reintroducing the original 149
-- constraint would reject valid origin_type='external_issue' rows and break
-- external issue imports.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'external_issue', 'slack_chat', 'agent_create'));
