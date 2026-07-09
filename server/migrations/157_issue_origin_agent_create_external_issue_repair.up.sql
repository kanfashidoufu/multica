-- Repair databases that applied the original 149_issue_origin_agent_create
-- migration before it was corrected to preserve origin_type='external_issue'.
-- Fresh databases already get this invariant from 149; this is intentionally
-- idempotent so both paths converge on the same CHECK list.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'external_issue', 'slack_chat', 'agent_create'));
