-- Management-catalog concurrency and provider probe metadata. Migration 0005
-- is intentionally reserved for the OIDC implementation.

ALTER TABLE providers
    ADD COLUMN cloud_project text,
    ADD COLUMN deployment text,
    ADD COLUMN connector_ready boolean NOT NULL DEFAULT true,
    ADD COLUMN last_probe_at timestamptz,
    ADD COLUMN last_probe_status text
        CHECK (last_probe_status IS NULL OR last_probe_status IN ('succeeded', 'failed')),
    ADD COLUMN last_probe_detail text
        CHECK (last_probe_detail IS NULL OR char_length(last_probe_detail) <= 500);

ALTER TABLE api_keys
    ADD COLUMN etag uuid NOT NULL DEFAULT uuidv7(),
    ADD COLUMN rotated_at timestamptz;

CREATE INDEX providers_created_cursor_idx ON providers (created_at, id);
CREATE INDEX provider_models_provider_cursor_idx ON provider_models (provider_id, created_at, id);
CREATE INDEX route_drafts_created_cursor_idx ON route_drafts (created_at, id);
CREATE INDEX route_revisions_route_cursor_idx ON route_revisions (route_id, revision DESC);
CREATE INDEX api_keys_created_cursor_idx ON api_keys (created_at, id);
