ALTER TABLE prices
    DROP CONSTRAINT prices_provider_kind_check,
    ADD CONSTRAINT prices_provider_kind_check CHECK (
        provider_kind IN (
            'open_ai', 'anthropic', 'anthropic_compatible', 'gemini', 'vertex_ai', 'bedrock',
            'azure_open_ai', 'open_ai_compatible'
        )
    );
