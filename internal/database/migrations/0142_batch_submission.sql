ALTER TABLE olp.batch_settlements
    ADD COLUMN IF NOT EXISTS submission jsonb,
    ADD COLUMN IF NOT EXISTS submission_source text,
    ADD COLUMN IF NOT EXISTS creator_deadline timestamptz;

UPDATE olp.batch_settlements b
SET submission = jsonb_build_object(
        'type', CASE
            WHEN l.legacy ? 'anthropic' THEN 'anthropic'
            WHEN l.legacy ? 'gemini_inline' THEN 'gemini_inline'
            WHEN l.legacy ? 'vertex_label' THEN 'vertex'
            WHEN l.legacy ? 'bedrock_client_request_token' THEN 'bedrock'
            WHEN l.legacy ? 'gemini_file' AND b.adapter = 'gemini_managed' THEN 'gemini_managed'
            WHEN l.legacy ? 'gemini_file' THEN 'gemini_file'
            ELSE 'files'
        END,
        'identity', l.legacy - 'creator_deadline' - 'anthropic' - 'gemini_inline'),
    submission_source = COALESCE(NULLIF(l.legacy->>'gemini_file', ''), NULLIF(l.legacy->>'input_file_id', '')),
    creator_deadline = CASE
        WHEN l.legacy->>'creator_deadline' ~ '^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$' THEN
            CASE
                WHEN (l.legacy->>'creator_deadline')::timestamptz > b.created_at THEN (l.legacy->>'creator_deadline')::timestamptz
                ELSE b.close_after
            END
        ELSE b.close_after
    END
FROM (
    SELECT resource_id, cursor->'submission' AS legacy
    FROM olp.batch_settlements
    WHERE jsonb_typeof(cursor->'submission') = 'object'
) l
WHERE b.resource_id = l.resource_id;

UPDATE olp.batch_settlements
SET cursor = cursor - 'submission'
WHERE cursor ? 'submission';

UPDATE olp.batch_settlements
SET creator_deadline = close_after
WHERE creator_deadline IS NULL;

ALTER TABLE olp.batch_settlements
    ALTER COLUMN creator_deadline SET NOT NULL;

DROP INDEX IF EXISTS olp.batch_settlements_unmapped_source;
CREATE INDEX IF NOT EXISTS batch_settlements_unmapped_submission_source
    ON olp.batch_settlements(provider_id, slot_id, submission_source, credential_id)
    WHERE upstream_id IS NULL;
