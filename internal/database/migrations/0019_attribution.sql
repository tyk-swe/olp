ALTER TABLE olp.requests
    ADD COLUMN attribution jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(attribution) = 'object'
               AND octet_length(attribution::text) <= 1024);
ALTER TABLE olp.attempt_usage_facts
    ADD COLUMN attribution jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(attribution) = 'object'
               AND octet_length(attribution::text) <= 1024);
ALTER TABLE olp.attempt_usage_hourly
    ADD COLUMN attribution jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(attribution) = 'object'
               AND octet_length(attribution::text) <= 1024);
ALTER TABLE olp.attempt_usage_hourly
    DROP CONSTRAINT attempt_usage_hourly_dimensions_key;
ALTER TABLE olp.attempt_usage_hourly
    ADD CONSTRAINT attempt_usage_hourly_dimensions_key
        UNIQUE NULLS NOT DISTINCT
        (bucket, route_slug, provider_id, upstream_model, operation, surface,
         api_key_id, budget_group_id, attribution);
