-- A runtime release must be able to prove that connector transport settings
-- still match the configuration it was compiled from. Provider endpoint and
-- cloud/auth settings deliberately stay out of the public core snapshot, so
-- retain their exact release-time values in an internal sidecar.
CREATE TABLE runtime_generation_provider_configs (
    runtime_generation_id uuid NOT NULL
        REFERENCES runtime_generations(id) ON DELETE CASCADE,
    provider_id uuid NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
    kind text NOT NULL,
    endpoint text,
    cloud_region text,
    cloud_project text,
    deployment text,
    api_version text,
    auth_mode text NOT NULL,
    active_credential_version_id uuid,
    PRIMARY KEY (runtime_generation_id, provider_id),
    FOREIGN KEY (active_credential_version_id)
        REFERENCES provider_credential_versions(id) ON DELETE RESTRICT
);

CREATE INDEX runtime_generation_provider_configs_provider_idx
    ON runtime_generation_provider_configs(provider_id, runtime_generation_id);

-- Preserve upgrade viability only where PostgreSQL can prove the current
-- provider row has not changed since a historical release was published.
-- Candidates with any missing provider sidecar remain ineligible; future
-- publications always write a complete sidecar transactionally.
INSERT INTO runtime_generation_provider_configs (
    runtime_generation_id,
    provider_id,
    kind,
    endpoint,
    cloud_region,
    cloud_project,
    deployment,
    api_version,
    auth_mode,
    active_credential_version_id
)
SELECT
    rg.id,
    p.id,
    p.kind,
    p.endpoint,
    p.cloud_region,
    p.cloud_project,
    p.deployment,
    p.api_version,
    p.auth_mode,
    p.active_credential_version_id
FROM runtime_generations rg
JOIN providers p
  ON p.state = 'active'::provider_state
 AND p.created_at <= rg.created_at
 AND p.updated_at <= rg.created_at;
