-- Recovery must distinguish competing intents sharing an upstream file and
-- notice when a gateway response has already mapped a listed provider job.
CREATE INDEX batch_settlements_unmapped_source
    ON olp.batch_settlements(provider_id, slot_id,
        (COALESCE(NULLIF(cursor#>>'{submission,gemini_file}',''), NULLIF(cursor#>>'{submission,input_file_id}',''))), credential_id)
    WHERE upstream_id IS NULL AND state IN ('submitting','submission_unknown','conflict','uncertain');

CREATE INDEX batch_settlements_upstream_source
    ON olp.batch_settlements(provider_id, slot_id, upstream_id, credential_id)
    WHERE upstream_id IS NOT NULL;
