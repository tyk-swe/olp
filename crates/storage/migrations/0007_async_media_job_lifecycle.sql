ALTER TABLE async_media_jobs
    ADD COLUMN provider_model text NOT NULL DEFAULT '',
    ADD COLUMN surface text NOT NULL DEFAULT 'openai',
    ADD COLUMN progress_percent numeric(5, 2),
    ADD COLUMN content_available boolean NOT NULL DEFAULT false,
    ADD COLUMN completed_at timestamptz,
    ADD COLUMN last_polled_at timestamptz,
    ADD COLUMN etag uuid NOT NULL DEFAULT uuidv7(),
    ADD CONSTRAINT async_media_jobs_surface_check
        CHECK (surface IN ('openai', 'anthropic', 'gemini')),
    ADD CONSTRAINT async_media_jobs_progress_check
        CHECK (progress_percent IS NULL OR progress_percent BETWEEN 0 AND 100),
    ADD CONSTRAINT async_media_jobs_completion_check
        CHECK (
            (state IN ('succeeded', 'failed', 'cancelled') AND completed_at IS NOT NULL)
            OR (state IN ('queued', 'running') AND completed_at IS NULL)
        );

ALTER TABLE async_media_jobs ALTER COLUMN provider_model DROP DEFAULT;

CREATE UNIQUE INDEX async_media_jobs_upstream_unique_idx
    ON async_media_jobs(provider_id, upstream_job_id)
    WHERE upstream_job_id IS NOT NULL;
CREATE INDEX async_media_jobs_created_idx
    ON async_media_jobs(created_at DESC, id DESC);
CREATE INDEX async_media_jobs_api_key_created_idx
    ON async_media_jobs(api_key_id, created_at DESC, id DESC);
CREATE INDEX async_media_jobs_state_created_idx
    ON async_media_jobs(state, created_at DESC, id DESC);

CREATE OR REPLACE FUNCTION enforce_media_job_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state IN ('succeeded', 'failed', 'cancelled') AND NEW.state <> OLD.state THEN
        RAISE EXCEPTION 'terminal media job state cannot transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'queued' AND NEW.state NOT IN ('queued', 'running', 'succeeded', 'failed', 'cancelled') THEN
        RAISE EXCEPTION 'invalid queued media job transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'running' AND NEW.state NOT IN ('running', 'succeeded', 'failed', 'cancelled') THEN
        RAISE EXCEPTION 'invalid running media job transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.progress_percent IS NOT NULL
       AND OLD.progress_percent IS NOT NULL
       AND NEW.progress_percent < OLD.progress_percent
    THEN
        RAISE EXCEPTION 'media job progress cannot decrease'
            USING ERRCODE = 'check_violation';
    END IF;
    NEW.updated_at := now();
    IF NEW.state IN ('succeeded', 'failed', 'cancelled') THEN
        NEW.completed_at := COALESCE(NEW.completed_at, now());
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER async_media_jobs_transition_guard
BEFORE UPDATE ON async_media_jobs
FOR EACH ROW EXECUTE FUNCTION enforce_media_job_transition();
