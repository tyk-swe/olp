ALTER TABLE olp_v3.route_drafts ADD COLUMN routing_policy jsonb NOT NULL DEFAULT '{}';
ALTER TABLE olp_v3.route_revisions ADD COLUMN routing_policy jsonb NOT NULL DEFAULT '{}';
ALTER TABLE olp_v3.api_keys ADD COLUMN routing_policy jsonb NOT NULL DEFAULT '{}';
CREATE TABLE olp_v3.routing_settings (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    policy jsonb NOT NULL DEFAULT '{}',
    etag uuid NOT NULL DEFAULT uuidv7()
);
INSERT INTO olp_v3.routing_settings DEFAULT VALUES;

ALTER TABLE olp_v3.attempts ADD COLUMN routing jsonb;

ALTER TABLE olp_v3.prices ADD COLUMN vendor_id text;
ALTER TABLE olp_v3.prices DROP CONSTRAINT prices_revision_scope_key;
ALTER TABLE olp_v3.prices ADD CONSTRAINT prices_revision_scope_key UNIQUE NULLS NOT DISTINCT(pricing_revision_id,provider_kind,provider_id,vendor_id,model,operation);

ALTER TABLE olp_v3.async_media_jobs ADD COLUMN credential_version_id uuid REFERENCES olp_v3.provider_credential_versions(id) ON DELETE RESTRICT;
UPDATE olp_v3.async_media_jobs j SET credential_version_id=pr.credential_version_id FROM olp_v3.provider_revisions pr WHERE pr.id=j.provider_revision_id;
