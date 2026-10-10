-- Enrollment binds grants to their plugin build and profile. Existing grants
-- have no reliable profile provenance and must be enrolled again.
ALTER TABLE olp.provider_credentials ADD COLUMN profile_id text
    CHECK (profile_id IS NULL OR (plugin_digest IS NOT NULL AND profile_id <> ''));

CREATE INDEX provider_resources_credential
    ON olp.provider_resources(credential_id, provider_id, created_at DESC, id DESC)
    WHERE credential_id IS NOT NULL AND state <> 'deleted';
