-- Plugin providers (ADR 0007). No vendor list price applies to a provider whose
-- profile a plugin supplies, so its attempts stay unpriced until an operator
-- sets a price scoped to that provider.
ALTER TABLE olp.prices
    DROP CONSTRAINT prices_provider_kind_check,
    ADD CONSTRAINT prices_provider_kind_check CHECK (provider_kind IN (
        'openai','anthropic','gemini','vertex_ai','bedrock','azure_openai','openai_compatible','plugin')),
    ADD CONSTRAINT prices_plugin_provider_check CHECK (provider_kind <> 'plugin' OR provider_id IS NOT NULL);
