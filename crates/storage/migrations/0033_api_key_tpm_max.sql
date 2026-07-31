UPDATE usage_facts
   SET cached_input_tokens = CASE
       WHEN input_tokens IS NULL THEN NULL
       ELSE LEAST(cached_input_tokens, input_tokens)
   END
 WHERE cached_input_tokens IS NOT NULL
   AND (input_tokens IS NULL OR cached_input_tokens > input_tokens);

ALTER TABLE usage_facts
VALIDATE CONSTRAINT usage_facts_cached_input_within_input;

SET LOCAL olp.usage_rollup_writer = 'additive-v2';

UPDATE usage_hourly
   SET cached_input_tokens = input_tokens
 WHERE cached_input_tokens > input_tokens;

ALTER TABLE usage_hourly
VALIDATE CONSTRAINT usage_hourly_cached_input_within_input;

UPDATE api_keys
   SET tokens_per_minute = 9007199254740991
 WHERE tokens_per_minute > 9007199254740991;

ALTER TABLE api_keys
ADD CONSTRAINT api_keys_tokens_per_minute_safe_integer
CHECK (
    tokens_per_minute IS NULL
    OR tokens_per_minute <= 9007199254740991
) NOT VALID;

ALTER TABLE api_keys
VALIDATE CONSTRAINT api_keys_tokens_per_minute_safe_integer;
