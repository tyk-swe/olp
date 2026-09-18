-- Durable media jobs. Job state is client-visible; lifecycle_state tracks
-- provider identity ownership independently of what the client sees.
-- Transition guards mirror olp_v3 so Go and Rust enforce identical invariants.
CREATE FUNCTION olp_go.enforce_media_job_lifecycle_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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

CREATE FUNCTION olp_go.enforce_media_job_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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

CREATE TABLE olp_go.media_jobs (
    id uuid PRIMARY KEY,
    upstream_job_id text,
    api_key_id uuid NOT NULL REFERENCES olp_go.api_keys,
    provider_id uuid NOT NULL REFERENCES olp_go.providers,
    provider_model text NOT NULL,
    route_slug text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL DEFAULT 'openai'
        CHECK (surface IN ('openai','anthropic','gemini')),
    state text NOT NULL
        CHECK (state IN ('queued','running','succeeded','failed','cancelled')),
    lifecycle_state text NOT NULL DEFAULT 'active'
        CHECK (lifecycle_state IN
            ('creating','active','create_ambiguous','create_cleanup_pending','delete_pending','deleted')),
    progress_percent numeric(5,2)
        CHECK (progress_percent IS NULL OR (progress_percent >= 0 AND progress_percent <= 100)),
    content_available boolean NOT NULL DEFAULT false,
    expires_at timestamptz,
    error_class text,
    completed_at timestamptz,
    last_polled_at timestamptz,
    reconciliation_error text,
    deleted_at timestamptz,
    etag uuid NOT NULL,
    runtime_generation_id uuid NOT NULL REFERENCES olp_go.runtime_releases,
    provider_revision_id uuid NOT NULL REFERENCES olp_go.provider_revisions,
    credential_version_id uuid REFERENCES olp_go.provider_credentials,
    reconciliation_claim_id uuid,
    reconciliation_claimed_until timestamptz,
    reconciliation_attempts integer NOT NULL DEFAULT 0 CHECK (reconciliation_attempts >= 0),
    next_reconciliation_at timestamptz NOT NULL DEFAULT now(),
    last_reconciliation_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (((state IN ('succeeded','failed','cancelled')) AND completed_at IS NOT NULL)
        OR ((state IN ('queued','running')) AND completed_at IS NULL)),
    CHECK ((lifecycle_state = 'deleted' AND deleted_at IS NOT NULL)
        OR (lifecycle_state <> 'deleted' AND deleted_at IS NULL)),
    CHECK ((reconciliation_claim_id IS NULL AND reconciliation_claimed_until IS NULL)
        OR (reconciliation_claim_id IS NOT NULL AND reconciliation_claimed_until IS NOT NULL)),
    CHECK (lifecycle_state IN ('creating','create_ambiguous','deleted') OR upstream_job_id IS NOT NULL)
);
CREATE UNIQUE INDEX media_jobs_upstream_unique_idx ON olp_go.media_jobs
    (provider_id, upstream_job_id) WHERE upstream_job_id IS NOT NULL;
CREATE INDEX media_jobs_api_key_created_idx ON olp_go.media_jobs
    (api_key_id, created_at DESC, id DESC);
CREATE INDEX media_jobs_created_idx ON olp_go.media_jobs (created_at DESC, id DESC);
CREATE INDEX media_jobs_state_created_idx ON olp_go.media_jobs
    (state, created_at DESC, id DESC);
CREATE INDEX media_jobs_provider_live_idx ON olp_go.media_jobs
    (provider_id, provider_revision_id) WHERE lifecycle_state <> 'deleted';
CREATE INDEX media_jobs_reconciliation_due_idx ON olp_go.media_jobs
    (next_reconciliation_at, created_at, id) WHERE lifecycle_state <> 'deleted';
CREATE INDEX media_jobs_reconciliation_idx ON olp_go.media_jobs
    (lifecycle_state, updated_at, id)
    WHERE lifecycle_state NOT IN ('active','deleted');
CREATE TRIGGER media_jobs_lifecycle_guard BEFORE UPDATE ON olp_go.media_jobs
    FOR EACH ROW EXECUTE FUNCTION olp_go.enforce_media_job_lifecycle_transition();
CREATE TRIGGER media_jobs_transition_guard BEFORE UPDATE ON olp_go.media_jobs
    FOR EACH ROW EXECUTE FUNCTION olp_go.enforce_media_job_transition();

ALTER TABLE olp_go.worker_task_health DROP CONSTRAINT worker_task_health_task_check;
ALTER TABLE olp_go.worker_task_health
    ADD CONSTRAINT worker_task_health_task_check CHECK (task IN (
        'request_metadata_consumer','maintenance','cost_reconciliation',
        'request_metadata_gateway_epoch_detection','media_reconciliation'));

ALTER TABLE olp_go.async_worker_counters
    ADD COLUMN media_reconciliation_gaps_total bigint NOT NULL DEFAULT 0
        CHECK (media_reconciliation_gaps_total >= 0);
