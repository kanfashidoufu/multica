CREATE OR REPLACE FUNCTION local_record_syndra_completion() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE outcome_text text;
BEGIN
    IF TG_OP='DELETE' THEN
        DELETE FROM local_syndra_bug_completion WHERE issue_id = OLD.id;
        RETURN NULL;
    END IF;
    IF current_setting('multica.syndra_mirror', true) = 'on' THEN RETURN NULL; END IF;
    IF issue_effective_status(NEW.workspace_id, NEW.status) <> 'done' THEN
        DELETE FROM local_syndra_bug_completion WHERE issue_id = NEW.id;
        RETURN NULL;
    END IF;
    IF issue_effective_status(OLD.workspace_id, OLD.status) = 'done'
       OR NEW.metadata->>'external_source' IS DISTINCT FROM 'syndra'
       OR NEW.metadata->>'bug_entity_type' IS DISTINCT FROM 'version_bug'
       OR NEW.origin_id IS NULL THEN RETURN NULL; END IF;

    -- Only export an explicitly designated comment or the public final task
    -- response. Never use arbitrary discussion, prompts or execution logs.
    SELECT NULLIF(btrim(split_part(replace(regexp_replace(c.content, '^\s*(处理结果|修复结果)\s*[:：]\s*', ''), E'\r\n', E'\n'), E'\n\n', 1)), '')
    INTO outcome_text FROM comment c
    WHERE c.workspace_id = NEW.workspace_id AND c.issue_id = NEW.id AND c.deleted_at IS NULL
      AND c.type = 'comment' AND c.author_type IN ('member','agent')
      AND c.content ~ '^\s*(处理结果|修复结果)\s*[:：]\s*\S'
    ORDER BY c.created_at DESC, c.id DESC LIMIT 1;
    IF outcome_text IS NULL THEN
        SELECT NULLIF(btrim(t.result->>'comment'), '') INTO outcome_text
        FROM agent_task_queue t JOIN agent a ON a.id = t.agent_id
        WHERE a.workspace_id = NEW.workspace_id AND t.issue_id = NEW.id AND t.status = 'completed'
          AND jsonb_typeof(t.result->'comment') = 'string' AND btrim(t.result->>'comment') <> ''
        ORDER BY t.completed_at DESC NULLS LAST, t.id DESC LIMIT 1;
    END IF;
    INSERT INTO local_syndra_bug_completion
        (issue_id,workspace_id,origin_id,revision,source_env,bug_id,record_key,reopen_count,outcome)
    VALUES (NEW.id,NEW.workspace_id,NEW.origin_id,NEW.revision,COALESCE(NEW.metadata->>'sync_source_env',''),
        COALESCE(NEW.metadata->>'bug_id',''),COALESCE(NEW.metadata->>'external_key',''),
        COALESCE(NEW.metadata->>'bug_reopen_count','0'),
        left(COALESCE(outcome_text,'Multica 已确认完成，未填写处理详情。'),4000))
    ON CONFLICT (issue_id) DO UPDATE SET event_id=EXCLUDED.event_id, revision=EXCLUDED.revision,
        workspace_id=EXCLUDED.workspace_id, origin_id=EXCLUDED.origin_id, source_env=EXCLUDED.source_env,
        bug_id=EXCLUDED.bug_id, record_key=EXCLUDED.record_key, reopen_count=EXCLUDED.reopen_count,
        outcome=EXCLUDED.outcome, attempts=0, available_at=now();
    RETURN NULL;
END $$;
DROP TRIGGER IF EXISTS local_syndra_completion_changed ON issue;
CREATE TRIGGER local_syndra_completion_changed AFTER UPDATE OF status ON issue
    FOR EACH ROW WHEN (NEW.origin_type='external_issue' AND OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION local_record_syndra_completion();
DROP TRIGGER IF EXISTS local_syndra_completion_deleted ON issue;
CREATE TRIGGER local_syndra_completion_deleted AFTER DELETE ON issue
    FOR EACH ROW WHEN (OLD.origin_type='external_issue')
    EXECUTE FUNCTION local_record_syndra_completion();
