-- name: ClaimSyndraCompletion :one
WITH candidate AS (
    SELECT p.issue_id FROM local_syndra_bug_completion p
    WHERE p.workspace_id = sqlc.arg('workspace_id') AND p.available_at <= now()
    ORDER BY p.available_at, p.issue_id LIMIT 1 FOR UPDATE SKIP LOCKED
)
UPDATE local_syndra_bug_completion AS q
SET attempts=q.attempts+1, available_at=now()+interval '1 second' * LEAST(900, 15 * (1 << LEAST(q.attempts,6)))
FROM candidate WHERE q.issue_id=candidate.issue_id RETURNING q.*;

-- name: SyndraCompletionIsDeliverable :one
SELECT EXISTS (
    SELECT 1 FROM local_syndra_bug_completion q JOIN issue i
        ON i.id = q.issue_id AND i.workspace_id = q.workspace_id AND i.origin_id = q.origin_id
    WHERE i.origin_type = 'external_issue' AND q.workspace_id = sqlc.arg('workspace_id')
      AND q.issue_id = sqlc.arg('issue_id') AND q.event_id = sqlc.arg('event_id')
)::boolean;

-- name: AckSyndraCompletion :exec
DELETE FROM local_syndra_bug_completion
WHERE workspace_id = sqlc.arg('workspace_id') AND issue_id = sqlc.arg('issue_id') AND event_id = sqlc.arg('event_id');
