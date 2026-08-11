-- Pricing lookup joins exact provider-kind and canonical-operation strings.
-- Reject an upgrade containing misspelled dimensions instead of retaining
-- revisions that can never price a usage fact.
ALTER TABLE prices
    ADD CONSTRAINT prices_provider_kind_check CHECK (
        provider_kind IN (
            'open_ai', 'anthropic', 'gemini', 'vertex_ai', 'bedrock',
            'azure_open_ai', 'open_ai_compatible'
        )
    ),
    ADD CONSTRAINT prices_operation_check CHECK (
        operation IN (
            'generation', 'embeddings', 'token_count', 'image_generation',
            'image_edit', 'image_variation', 'speech', 'transcription',
            'video_create', 'video_list', 'video_get', 'video_content',
            'video_delete', 'moderation', 'model_list', 'model_get'
        )
    );
