-- Azure Search supports the same explicit rate-card and discount scopes as
-- provider cost prices. Keep every pricing-kind constraint in agreement.
ALTER TABLE olp.rate_card_prices DROP CONSTRAINT rate_card_prices_provider_kind_check;
ALTER TABLE olp.rate_card_prices ADD CONSTRAINT rate_card_prices_provider_kind_check CHECK (provider_kind IN (
 'openai','anthropic','gemini','vertex_ai','bedrock','sagemaker','watsonx','azure_openai','azure_ai_search','openai_compatible','plugin'));
ALTER TABLE olp.pricing_discounts DROP CONSTRAINT pricing_discounts_provider_kind_check;
ALTER TABLE olp.pricing_discounts ADD CONSTRAINT pricing_discounts_provider_kind_check CHECK (provider_kind IN (
 'openai','anthropic','gemini','vertex_ai','bedrock','sagemaker','watsonx','azure_openai','azure_ai_search','openai_compatible','plugin'));
