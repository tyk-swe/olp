-- Gemini Interactions are provider-owned state, separate from GenerateContent
-- and strict Responses. Older readers do not recognize this kind and fail
-- closed. The provider's actual ID is encrypted with the existing secret
-- authority; upstream_id is only this row's internal UUID.
ALTER TABLE olp_go.provider_resources DROP CONSTRAINT provider_resources_kind_check;
ALTER TABLE olp_go.provider_resources ADD CONSTRAINT provider_resources_kind_check
    CHECK (kind IN ('file', 'batch', 'response', 'continuation', 'strict_response', 'interaction'));

ALTER TABLE olp_go.provider_resources ADD CONSTRAINT provider_resources_interaction_contract_check
    CHECK (kind <> 'interaction' OR (
        contract_version IS NOT NULL
        AND contract_version = 'gemini-interaction/v1beta'
        AND expires_at IS NOT NULL
        AND expires_at <= created_at + interval '24 hours'
        AND upstream_id = id::text
        AND submission_id IS NULL
    ));
