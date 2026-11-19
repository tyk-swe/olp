-- SageMaker AI real-time endpoints are a connector kind, so a price may be
-- scoped to them like any other kind.
ALTER TABLE olp.prices
    DROP CONSTRAINT prices_provider_kind_check,
    ADD CONSTRAINT prices_provider_kind_check CHECK (provider_kind IN (
        'openai','anthropic','gemini','vertex_ai','bedrock','sagemaker','azure_openai','openai_compatible','plugin'));
