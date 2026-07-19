-- Async provider jobs outlive the request and may outlive the API key that
-- created them.  Bind new reservations to the immutable provider authority
-- selected by their runtime generation, and add a short database lease so
-- every gateway can participate in bounded, restart-safe reconciliation.
ALTER TABLE async_media_jobs
    ADD COLUMN runtime_generation_id uuid REFERENCES runtime_generations(id) ON DELETE RESTRICT,
    ADD COLUMN provider_revision_id uuid REFERENCES provider_revisions(id) ON DELETE RESTRICT,
    ADD COLUMN reconciliation_claim_id uuid,
    ADD COLUMN reconciliation_claimed_until timestamptz,
    ADD COLUMN reconciliation_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN next_reconciliation_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN last_reconciliation_at timestamptz,
    ADD CONSTRAINT async_media_jobs_authority_pair_check CHECK (
        (runtime_generation_id IS NULL) = (provider_revision_id IS NULL)
    ),
    ADD CONSTRAINT async_media_jobs_reconciliation_attempts_check
        CHECK (reconciliation_attempts >= 0),
    ADD CONSTRAINT async_media_jobs_reconciliation_claim_check CHECK (
        (reconciliation_claim_id IS NULL AND reconciliation_claimed_until IS NULL)
        OR (reconciliation_claim_id IS NOT NULL AND reconciliation_claimed_until IS NOT NULL)
    );

-- Best-effort authority backfill for pre-upgrade rows. The nearest release at
-- or before creation is the strongest evidence available. Rows without such
-- evidence remain explicitly unbound and are surfaced by reconciliation
-- health rather than being silently attached to a newer credential.
WITH authority AS (
    SELECT j.id, selected.runtime_generation_id, selected.provider_revision_id
    FROM async_media_jobs j
    CROSS JOIN LATERAL (
        SELECT rpc.runtime_generation_id, rpc.provider_revision_id
        FROM runtime_generation_provider_configs rpc
        JOIN runtime_generations rg ON rg.id = rpc.runtime_generation_id
        WHERE rpc.provider_id = j.provider_id
          AND rpc.provider_revision_id IS NOT NULL
          AND rg.created_at <= j.created_at
        ORDER BY rg.sequence DESC
        LIMIT 1
    ) selected
)
UPDATE async_media_jobs j
SET runtime_generation_id = authority.runtime_generation_id,
    provider_revision_id = authority.provider_revision_id
FROM authority
WHERE authority.id = j.id;

CREATE INDEX async_media_jobs_reconciliation_due_idx
    ON async_media_jobs (next_reconciliation_at, created_at, id)
    WHERE lifecycle_state <> 'deleted';

CREATE INDEX async_media_jobs_provider_live_idx
    ON async_media_jobs (provider_id, provider_revision_id)
    WHERE lifecycle_state <> 'deleted';
