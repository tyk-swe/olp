CREATE TABLE olp_go.provider_resources (
    id uuid PRIMARY KEY,
    kind text NOT NULL CHECK (kind IN ('file','batch','response')),
    api_key_id uuid NOT NULL REFERENCES olp_go.api_keys,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL REFERENCES olp_go.providers,
    provider_revision_id uuid NOT NULL,
    route_revision_id uuid NOT NULL,
    slot_id uuid NOT NULL,
    credential_id uuid REFERENCES olp_go.provider_credentials,
    upstream_id text NOT NULL,
    state text NOT NULL,
    metadata jsonb NOT NULL DEFAULT '{}',
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(kind,provider_id,upstream_id),
    CHECK (octet_length(upstream_id) BETWEEN 1 AND 512),
    CHECK (octet_length(metadata::text)<=16384)
);
CREATE INDEX provider_resources_owner ON olp_go.provider_resources(api_key_id,kind,created_at DESC,id DESC);

ALTER TABLE olp_go.prices DROP CONSTRAINT prices_operation_check;
ALTER TABLE olp_go.prices ADD CONSTRAINT prices_operation_check
    CHECK (operation IN (
        'generation','embeddings','token_count','image_generation','image_edit','image_variation',
        'speech','transcription','video_create','video_list','video_get','video_content',
        'video_delete','moderation','model_list','model_get','rerank',
        'batch','realtime','bedrock_invoke'));

ALTER TABLE olp_go.attempt_usage_facts DROP CONSTRAINT attempt_usage_facts_surface_check;
ALTER TABLE olp_go.attempt_usage_facts ADD CONSTRAINT attempt_usage_facts_surface_check
    CHECK (surface IN ('openai','anthropic','gemini','bedrock','unknown'));
ALTER TABLE olp_go.attempt_usage_hourly DROP CONSTRAINT attempt_usage_hourly_surface_check;
ALTER TABLE olp_go.attempt_usage_hourly ADD CONSTRAINT attempt_usage_hourly_surface_check
    CHECK (surface IN ('openai','anthropic','gemini','bedrock','unknown'));
