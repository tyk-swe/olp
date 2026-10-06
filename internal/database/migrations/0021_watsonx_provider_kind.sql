-- IBM watsonx.ai is a connector kind, so a price may be scoped to it like any
-- other kind.
ALTER TABLE olp.prices
    DROP CONSTRAINT prices_provider_kind_check,
    ADD CONSTRAINT prices_provider_kind_check CHECK (provider_kind IN (
        'openai','anthropic','gemini','vertex_ai','bedrock','sagemaker','watsonx','azure_openai','openai_compatible','plugin'));
