ALTER TABLE usage_facts
ADD CONSTRAINT usage_facts_cached_input_within_input
CHECK (
    cached_input_tokens IS NULL
    OR (input_tokens IS NOT NULL AND cached_input_tokens <= input_tokens)
) NOT VALID;

ALTER TABLE usage_hourly
ADD CONSTRAINT usage_hourly_cached_input_within_input
CHECK (cached_input_tokens <= input_tokens) NOT VALID;
