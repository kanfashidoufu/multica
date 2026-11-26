CREATE TABLE IF NOT EXISTS local_syndra_bug_completion (
    issue_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    origin_id uuid NOT NULL,
    event_id uuid NOT NULL DEFAULT gen_random_uuid(),
    revision bigint NOT NULL,
    source_env text NOT NULL,
    bug_id text NOT NULL,
    record_key text NOT NULL,
    reopen_count text NOT NULL,
    outcome text NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now()
);
