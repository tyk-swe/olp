-- Recoverable interaction state remains owned by provider_resources. The
-- payload lives under the same UUID in the existing encrypted secrets store.
ALTER TABLE olp_go.provider_resources
    DROP CONSTRAINT provider_resources_kind_check;
ALTER TABLE olp_go.provider_resources
    ADD CONSTRAINT provider_resources_kind_check
        CHECK (kind IN ('file', 'batch', 'response', 'continuation', 'strict_response'));

ALTER TABLE olp_go.provider_resources
    ADD COLUMN contract_version text,
    ADD COLUMN parent_id uuid,
    ADD COLUMN submission_id text;

ALTER TABLE olp_go.provider_resources
    ADD CONSTRAINT provider_resources_contract_version_check
        CHECK (contract_version IS NULL OR octet_length(contract_version) BETWEEN 1 AND 128),
    ADD CONSTRAINT provider_resources_continuation_contract_check
        CHECK (kind NOT IN ('continuation', 'strict_response')
               OR (contract_version IS NOT NULL AND expires_at IS NOT NULL)),
    ADD CONSTRAINT provider_resources_submission_check
        CHECK (submission_id IS NULL
               OR (kind = 'continuation' AND octet_length(submission_id) BETWEEN 1 AND 128));

-- A retried client submission cannot select another provider or route and
-- perform fresh inference. The owner still authorizes every lookup.
CREATE UNIQUE INDEX provider_resources_submission_identity
    ON olp_go.provider_resources(api_key_id, submission_id)
    WHERE submission_id IS NOT NULL;
CREATE INDEX provider_resources_parent
    ON olp_go.provider_resources(api_key_id, parent_id)
    WHERE parent_id IS NOT NULL;

COMMENT ON COLUMN olp_go.provider_resources.parent_id IS
    'Immutable logical lineage, checked by the resource owner on creation. It may outlive an expired parent; children contain complete bounded state and do not retain parent payloads by foreign key.';
COMMENT ON COLUMN olp_go.provider_resources.contract_version IS
    'Version of the resource-owned encrypted interaction contract. New strict resource kinds prevent older readers from treating these records as legacy mappings.';
COMMENT ON COLUMN olp_go.provider_resources.submission_id IS
    'Owner-scoped client submission identity for accepted-work and delivery replay. Never an authorization credential.';

ALTER TABLE olp_go.secrets DROP CONSTRAINT secrets_purpose_check;
ALTER TABLE olp_go.secrets
    ADD CONSTRAINT secrets_purpose_check
        CHECK (purpose IN ('oidc_client', 'oidc_flow', 'mutation_replay',
                           'provider_credential', 'notification_secret',
                           'provider_continuation')),
    ADD CONSTRAINT secrets_continuation_ciphertext_bound
        CHECK (purpose <> 'provider_continuation'
               OR (expires_at IS NOT NULL AND octet_length(ciphertext) <= 5242880));
