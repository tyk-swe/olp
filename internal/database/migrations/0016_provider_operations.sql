ALTER TABLE olp_go.prices DROP CONSTRAINT prices_operation_check;
ALTER TABLE olp_go.prices ADD CONSTRAINT prices_operation_check
    CHECK (operation IN (
        'generation','embeddings','token_count','image_generation','image_edit','image_variation',
        'speech','transcription','video_create','video_list','video_get','video_content',
        'video_delete','moderation','model_list','model_get','rerank'));
