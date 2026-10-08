-- Only project-scoped HMAC digests enter durable accounting. Empty means the
-- key did not request identification. Retained aggregates preserve the scope.
ALTER TABLE olp.requests ADD COLUMN end_user_digest text NOT NULL DEFAULT ''
    CHECK (end_user_digest = '' OR end_user_digest ~ '^[0-9a-f]{64}$');
ALTER TABLE olp.attempt_usage_facts ADD COLUMN end_user_digest text NOT NULL DEFAULT ''
    CHECK (end_user_digest = '' OR end_user_digest ~ '^[0-9a-f]{64}$');
ALTER TABLE olp.attempt_usage_hourly ADD COLUMN end_user_digest text NOT NULL DEFAULT ''
    CHECK (end_user_digest = '' OR end_user_digest ~ '^[0-9a-f]{64}$'),
    DROP CONSTRAINT attempt_usage_hourly_dimensions_key,
    ADD CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
        (bucket, route_slug, provider_id, upstream_model, operation, surface,
         api_key_id, budget_group_id, attribution, model_family, estimate_provenance, end_user_digest);
