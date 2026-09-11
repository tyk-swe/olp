-- Default slots also carry policies when authentication uses no stored secret.
ALTER TABLE olp_v3.provider_revision_credentials
    ALTER COLUMN credential_version_id DROP NOT NULL;

-- Historical credentialless revisions had unrestricted implicit default slots.
INSERT INTO olp_v3.provider_revision_credentials
    (provider_revision_id, slot_id, credential_version_id, configuration)
SELECT pr.id, pr.provider_id, NULL,
       jsonb_build_object('id', pr.provider_id, 'name', 'Default', 'enabled', true,
                          'priority', 0, 'weight', 1, 'credential_version_id', NULL)
FROM olp_v3.provider_revisions pr
WHERE pr.credential_version_id IS NULL AND pr.auth_mode IN ('none', 'adc', 'default_chain')
ON CONFLICT DO NOTHING;
