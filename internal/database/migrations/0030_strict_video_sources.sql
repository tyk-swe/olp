-- A strict video job keeps bounded native provider metadata encrypted under
-- the existing API-key-owned job identity. The secret's authenticated plaintext
-- retains member order and exact numeric lexemes; jsonb would erase both.
-- The spool remains request-scoped and carries no durable media bytes.
ALTER TABLE olp_go.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp_go.secrets ADD CONSTRAINT secrets_purpose_check
    CHECK (purpose IN ('oidc_client', 'oidc_flow', 'mutation_replay',
                      'provider_credential', 'notification_secret',
                      'provider_continuation', 'media_job_source'));
ALTER TABLE olp_go.secrets ADD CONSTRAINT secrets_media_job_source_bound
    CHECK (purpose <> 'media_job_source' OR
           (expires_at IS NOT NULL AND octet_length(ciphertext) <= 1048604));

ALTER TABLE olp_go.media_jobs
    ADD COLUMN strict_contract boolean NOT NULL DEFAULT false,
    ADD COLUMN native_source_id uuid,
    ADD CONSTRAINT media_jobs_strict_source CHECK
        (NOT strict_contract OR lifecycle_state <> 'active' OR native_source_id IS NOT NULL);
