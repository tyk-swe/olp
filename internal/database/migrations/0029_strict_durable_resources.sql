-- Strict file and batch identities cannot be read as legacy metadata-only
-- resources by an older gateway. Their exact native source and latest result
-- are encrypted under the existing resource UUID and secret authority.
ALTER TABLE olp_go.provider_resources DROP CONSTRAINT provider_resources_kind_check;
ALTER TABLE olp_go.provider_resources ADD CONSTRAINT provider_resources_kind_check
    CHECK (kind IN ('file', 'batch', 'response', 'continuation', 'strict_response',
                   'interaction', 'strict_file', 'strict_batch'));

ALTER TABLE olp_go.provider_resources ADD CONSTRAINT provider_resources_strict_durable_check
    CHECK (kind NOT IN ('strict_file', 'strict_batch') OR (
        contract_version = 'native-durable-v1'
        AND expires_at IS NOT NULL
        AND expires_at <= created_at + interval '7 days'
        AND submission_id IS NULL
    ));
