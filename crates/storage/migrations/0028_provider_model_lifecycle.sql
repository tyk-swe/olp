-- Provider model inventories are observed independently from the immutable
-- runtime revision.  A discovery result must never make an active revision
-- disappear merely because an upstream list is transiently incomplete.
ALTER TABLE providers
    ADD COLUMN certification_context_id uuid NOT NULL DEFAULT uuidv7(),
    ADD COLUMN last_probe_context_id uuid,
    ADD COLUMN last_model_discovery_at timestamptz,
    ADD COLUMN last_model_discovery_status text,
    ADD COLUMN model_discovery_claim_id uuid,
    ADD COLUMN model_discovery_claimed_until timestamptz;

ALTER TABLE provider_models
    ADD COLUMN inventory_source text NOT NULL DEFAULT 'legacy'
        CHECK (inventory_source IN ('legacy', 'upstream', 'manual', 'configured')),
    ADD COLUMN availability text NOT NULL DEFAULT 'available'
        CHECK (availability IN ('available', 'missing')),
    ADD COLUMN first_seen_at timestamptz,
    ADD COLUMN last_seen_at timestamptz,
    ADD COLUMN missing_since timestamptz,
    ADD COLUMN consecutive_missing_runs integer NOT NULL DEFAULT 0
        CHECK (consecutive_missing_runs >= 0),
    ADD COLUMN review_revision uuid NOT NULL DEFAULT uuidv7();

UPDATE provider_models
SET first_seen_at = COALESCE(discovered_at, created_at),
    last_seen_at = COALESCE(discovered_at, created_at);

CREATE TABLE provider_model_discovery_runs (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    actor_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    origin text NOT NULL CHECK (origin IN ('manual', 'upstream', 'scheduled')),
    completeness text NOT NULL CHECK (completeness IN ('complete', 'partial')),
    status text NOT NULL CHECK (status IN ('succeeded', 'failed', 'superseded')),
    expected_etag uuid NOT NULL,
    observed_model_count integer NOT NULL CHECK (observed_model_count >= 0),
    added_model_count integer NOT NULL DEFAULT 0 CHECK (added_model_count >= 0),
    renamed_model_count integer NOT NULL DEFAULT 0 CHECK (renamed_model_count >= 0),
    missing_model_count integer NOT NULL DEFAULT 0 CHECK (missing_model_count >= 0),
    detail text NOT NULL DEFAULT '' CHECK (char_length(detail) <= 500),
    started_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX provider_model_discovery_runs_provider_completed_idx
    ON provider_model_discovery_runs(provider_id, completed_at DESC);

CREATE TABLE capability_certification_runs (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    provider_model_id uuid NOT NULL REFERENCES provider_models(id) ON DELETE CASCADE,
    actor_user_id uuid REFERENCES users(id) ON DELETE SET NULL,
    certification_context_id uuid NOT NULL,
    review_revision uuid NOT NULL,
    status text NOT NULL CHECK (status IN ('running', 'succeeded', 'partial', 'failed', 'superseded')),
    attempted_count integer NOT NULL CHECK (attempted_count > 0),
    certified_count integer NOT NULL DEFAULT 0 CHECK (certified_count >= 0),
    started_at timestamptz NOT NULL DEFAULT now(),
    lease_expires_at timestamptz NOT NULL,
    completed_at timestamptz
);
CREATE UNIQUE INDEX capability_certification_runs_active_context_idx
    ON capability_certification_runs(provider_model_id, certification_context_id, review_revision)
    WHERE status = 'running';
CREATE INDEX capability_certification_runs_model_completed_idx
    ON capability_certification_runs(provider_model_id, completed_at DESC);

CREATE TABLE capability_certification_results (
    certification_run_id uuid NOT NULL REFERENCES capability_certification_runs(id) ON DELETE CASCADE,
    operation text NOT NULL,
    surface text NOT NULL,
    mode text NOT NULL,
    succeeded boolean NOT NULL,
    evidence_kind text,
    error_code text,
    detail text NOT NULL CHECK (char_length(detail) <= 500),
    PRIMARY KEY (certification_run_id, operation, surface, mode),
    CHECK ((succeeded AND evidence_kind IS NOT NULL AND error_code IS NULL)
        OR (NOT succeeded AND evidence_kind IS NULL))
);

ALTER TABLE model_capabilities
    ADD COLUMN certification_context_id uuid,
    ADD COLUMN review_revision uuid,
    ADD COLUMN certification_run_id uuid,
    ADD COLUMN certification_evidence_kind text;

UPDATE model_capabilities mc
SET certification_context_id = p.certification_context_id,
    review_revision = pm.review_revision,
    certification_evidence_kind = 'legacy'
FROM provider_models pm
JOIN providers p ON p.id = pm.provider_id
WHERE mc.provider_model_id = pm.id
  AND mc.source = 'certified';

ALTER TABLE provider_revision_capabilities
    ADD COLUMN certification_context_id uuid,
    ADD COLUMN certification_run_id uuid,
    ADD COLUMN certification_evidence_kind text;

CREATE INDEX provider_models_provider_availability_idx
    ON provider_models(provider_id, availability, id);
