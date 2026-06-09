-- Allow external issue import webhooks to stamp idempotent source records
-- without overloading Lark chat or quick-create provenance.
ALTER TABLE issue DROP CONSTRAINT IF EXISTS issue_origin_type_check;
ALTER TABLE issue ADD CONSTRAINT issue_origin_type_check
    CHECK (origin_type IN ('autopilot', 'quick_create', 'lark_chat', 'external_issue'));

CREATE UNIQUE INDEX IF NOT EXISTS idx_issue_external_origin_unique
    ON issue(workspace_id, origin_type, origin_id)
    WHERE origin_type = 'external_issue';
