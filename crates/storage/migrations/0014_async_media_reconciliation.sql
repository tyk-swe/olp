-- Async media has an upstream side effect and a PostgreSQL metadata record.
-- Track their reconciliation independently from the provider-reported job
-- state so create/delete ambiguity remains visible without persisting prompts,
-- uploads, or generated content.
ALTER TABLE async_media_jobs
    ADD COLUMN lifecycle_state text NOT NULL DEFAULT 'active',
    ADD COLUMN reconciliation_error text,
    ADD COLUMN deleted_at timestamptz;

UPDATE async_media_jobs
SET lifecycle_state = 'create_ambiguous',
    reconciliation_error = 'legacy_row_missing_upstream_job_id'
WHERE upstream_job_id IS NULL;

ALTER TABLE async_media_jobs
    ADD CONSTRAINT async_media_jobs_lifecycle_state_check
        CHECK (lifecycle_state IN (
            'creating',
            'active',
            'create_ambiguous',
            'create_cleanup_pending',
            'delete_pending',
            'deleted'
        )),
    ADD CONSTRAINT async_media_jobs_upstream_binding_check
        CHECK (
            lifecycle_state IN ('creating', 'create_ambiguous', 'deleted')
            OR upstream_job_id IS NOT NULL
        ),
    ADD CONSTRAINT async_media_jobs_deleted_at_check
        CHECK (
            (lifecycle_state = 'deleted' AND deleted_at IS NOT NULL)
            OR (lifecycle_state <> 'deleted' AND deleted_at IS NULL)
        );

CREATE INDEX async_media_jobs_reconciliation_idx
    ON async_media_jobs(lifecycle_state, updated_at, id)
    WHERE lifecycle_state NOT IN ('active', 'deleted');

CREATE OR REPLACE FUNCTION enforce_media_job_lifecycle_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.lifecycle_state = 'deleted' AND NEW.lifecycle_state <> 'deleted' THEN
        RAISE EXCEPTION 'deleted media job lifecycle is terminal'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'creating'
       AND NEW.lifecycle_state NOT IN (
           'creating', 'active', 'create_ambiguous', 'create_cleanup_pending', 'deleted'
       )
    THEN
        RAISE EXCEPTION 'invalid creating media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'active'
       AND NEW.lifecycle_state NOT IN ('active', 'delete_pending')
    THEN
        RAISE EXCEPTION 'invalid active media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'create_ambiguous'
       AND NEW.lifecycle_state NOT IN (
           'create_ambiguous', 'create_cleanup_pending', 'deleted'
       )
    THEN
        RAISE EXCEPTION 'invalid ambiguous media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'create_cleanup_pending'
       AND NEW.lifecycle_state NOT IN ('create_cleanup_pending', 'deleted')
    THEN
        RAISE EXCEPTION 'invalid create cleanup media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'delete_pending'
       AND NEW.lifecycle_state NOT IN ('delete_pending', 'deleted')
    THEN
        RAISE EXCEPTION 'invalid delete-pending media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.lifecycle_state = 'deleted' THEN
        NEW.deleted_at := COALESCE(NEW.deleted_at, now());
        NEW.content_available := false;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER async_media_jobs_lifecycle_guard
BEFORE UPDATE ON async_media_jobs
FOR EACH ROW EXECUTE FUNCTION enforce_media_job_lifecycle_transition();
