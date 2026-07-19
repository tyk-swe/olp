-- Provider catalog rows are mutable drafts. Runtime authority is an immutable
-- activated revision so editing/testing a replacement can never leak through
-- an unrelated key, route, or settings publication.
CREATE TABLE provider_revisions (
    id uuid PRIMARY KEY,
    provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
    revision integer NOT NULL CHECK (revision > 0),
    name text NOT NULL,
    kind text NOT NULL,
    endpoint text,
    cloud_region text,
    cloud_project text,
    deployment text,
    api_version text,
    auth_mode text NOT NULL,
    connector_ready boolean NOT NULL,
    credential_version_id uuid REFERENCES provider_credential_versions(id) ON DELETE RESTRICT,
    source_etag uuid NOT NULL,
    activated_by uuid NOT NULL REFERENCES users(id),
    activated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider_id, revision),
    UNIQUE (provider_id, id),
    CONSTRAINT provider_revisions_credential_owner_fk
        FOREIGN KEY (provider_id, credential_version_id)
        REFERENCES provider_credential_versions(provider_id, id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE provider_revision_models (
    id uuid PRIMARY KEY,
    provider_revision_id uuid NOT NULL REFERENCES provider_revisions(id) ON DELETE RESTRICT,
    source_provider_model_id uuid NOT NULL REFERENCES provider_models(id) ON DELETE RESTRICT,
    upstream_model text NOT NULL,
    display_name text NOT NULL,
    enabled boolean NOT NULL,
    discovered_at timestamptz,
    UNIQUE (provider_revision_id, source_provider_model_id),
    UNIQUE (provider_revision_id, upstream_model)
);

CREATE TABLE provider_revision_capabilities (
    provider_revision_model_id uuid NOT NULL
        REFERENCES provider_revision_models(id) ON DELETE RESTRICT,
    operation text NOT NULL,
    surface text NOT NULL,
    mode text NOT NULL,
    source text NOT NULL CHECK (source IN ('declared', 'probed', 'certified')),
    certified_at timestamptz,
    PRIMARY KEY (provider_revision_model_id, operation, surface, mode),
    CONSTRAINT provider_revision_capabilities_evidence_check
        CHECK ((source = 'certified') = (certified_at IS NOT NULL))
);

ALTER TABLE providers
    ADD COLUMN active_revision_id uuid,
    ADD CONSTRAINT providers_active_revision_owner_fk
        FOREIGN KEY (id, active_revision_id)
        REFERENCES provider_revisions(provider_id, id)
        DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE runtime_generation_provider_configs
    ADD COLUMN provider_revision_id uuid REFERENCES provider_revisions(id) ON DELETE RESTRICT;

CREATE INDEX provider_revisions_provider_idx
    ON provider_revisions(provider_id, revision DESC);
CREATE INDEX provider_revision_models_source_idx
    ON provider_revision_models(source_provider_model_id, provider_revision_id);

-- Backfill the configuration that was already live before immutable revisions
-- existed. Prior activation is the migration certification boundary for those
-- exact tuples; all future revisions require fresh tuple evidence.
INSERT INTO provider_revisions (
    id, provider_id, revision, name, kind, endpoint, cloud_region, cloud_project,
    deployment, api_version, auth_mode, connector_ready, credential_version_id,
    source_etag, activated_by, activated_at
)
SELECT
    uuidv7(), p.id, 1, p.name, p.kind, p.endpoint, p.cloud_region, p.cloud_project,
    p.deployment, p.api_version, p.auth_mode, p.connector_ready,
    p.active_credential_version_id, p.etag, p.created_by, p.updated_at
FROM providers p
WHERE p.state = 'active'::provider_state;

INSERT INTO provider_revision_models (
    id, provider_revision_id, source_provider_model_id, upstream_model,
    display_name, enabled, discovered_at
)
SELECT
    uuidv7(), pr.id, pm.id, pm.upstream_model, pm.display_name, pm.enabled, pm.discovered_at
FROM provider_revisions pr
JOIN provider_models pm ON pm.provider_id = pr.provider_id;

INSERT INTO provider_revision_capabilities (
    provider_revision_model_id, operation, surface, mode, source, certified_at
)
SELECT prm.id, mc.operation, mc.surface, mc.mode, 'certified',
       COALESCE(mc.certified_at, pr.activated_at)
FROM provider_revision_models prm
JOIN provider_revisions pr ON pr.id = prm.provider_revision_id
JOIN model_capabilities mc ON mc.provider_model_id = prm.source_provider_model_id;

UPDATE providers p
SET active_revision_id = pr.id
FROM provider_revisions pr
WHERE pr.provider_id = p.id AND pr.revision = 1
  AND p.state = 'active'::provider_state;

-- Bind sidecars written by migration 0013 to the matching immutable revision
-- only when every transport-affecting value is identical.
UPDATE runtime_generation_provider_configs rpc
SET provider_revision_id = pr.id
FROM provider_revisions pr
WHERE pr.provider_id = rpc.provider_id
  AND pr.kind = rpc.kind
  AND pr.endpoint IS NOT DISTINCT FROM rpc.endpoint
  AND pr.cloud_region IS NOT DISTINCT FROM rpc.cloud_region
  AND pr.cloud_project IS NOT DISTINCT FROM rpc.cloud_project
  AND pr.deployment IS NOT DISTINCT FROM rpc.deployment
  AND pr.api_version IS NOT DISTINCT FROM rpc.api_version
  AND pr.auth_mode = rpc.auth_mode
  AND pr.credential_version_id IS NOT DISTINCT FROM rpc.active_credential_version_id;
