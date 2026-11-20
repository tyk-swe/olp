-- Result-view deletion looks up physical siblings across route revisions.
CREATE INDEX provider_resources_live_file_by_upstream
    ON olp.provider_resources(api_key_id, provider_id, slot_id, upstream_id, credential_id)
    WHERE kind = 'file' AND state <> 'deleted';

CREATE INDEX batch_settlements_pending_gemini
    ON olp.batch_settlements(api_key_id, provider_id, slot_id, credential_id, resource_id)
    WHERE adapter = 'gemini_managed' AND closed_at IS NULL;
