-- Each attempt records what admission estimated its input to be and the
-- tokenizer family of its model, so reports can set the estimate against the
-- usage the provider reported. An attempt that was never estimated, such as a
-- stored-response call or a job poll, has no estimate and an empty provenance
-- and still has its model's family; an empty family is only a row recorded
-- before this migration. The estimate and its provenance move together, so a
-- provenance never describes an estimate that is not there.
ALTER TABLE olp.attempt_usage_facts
    ADD COLUMN estimated_input_tokens bigint
        CHECK (estimated_input_tokens IS NULL OR estimated_input_tokens >= 0),
    ADD COLUMN estimate_provenance text NOT NULL DEFAULT ''
        CHECK (estimate_provenance IN ('', 'tokenizer', 'calibrated', 'heuristic')),
    ADD COLUMN model_family text NOT NULL DEFAULT ''
        CHECK (model_family = '' OR model_family ~ '^[a-z][a-z0-9_-]{0,63}$'),
    ADD CONSTRAINT attempt_usage_facts_estimate_check CHECK (
        (estimated_input_tokens IS NULL) = (estimate_provenance = ''));

-- Retained usage keeps estimation error reportable by family and provenance.
-- Both are dimensions, declared NOT NULL like the other text dimensions so a
-- rollup into an hour that already carries them collides instead of
-- duplicating. The three sums cover only the attempts that had an estimate and
-- reported usage, the attempts whose error can be known: estimated and reported
-- input tokens are summed over the same attempts, and the count says how many.
ALTER TABLE olp.attempt_usage_hourly
    ADD COLUMN estimate_provenance text NOT NULL DEFAULT ''
        CHECK (estimate_provenance IN ('', 'tokenizer', 'calibrated', 'heuristic')),
    ADD COLUMN model_family text NOT NULL DEFAULT ''
        CHECK (model_family = '' OR model_family ~ '^[a-z][a-z0-9_-]{0,63}$'),
    ADD COLUMN estimated_input_tokens numeric(30,0) NOT NULL DEFAULT 0
        CHECK (estimated_input_tokens >= 0),
    ADD COLUMN estimate_reported_input_tokens numeric(30,0) NOT NULL DEFAULT 0
        CHECK (estimate_reported_input_tokens >= 0),
    ADD COLUMN estimate_attempt_count bigint NOT NULL DEFAULT 0
        CHECK (estimate_attempt_count >= 0),
    DROP CONSTRAINT attempt_usage_hourly_dimensions_key,
    ADD CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
        (bucket, route_slug, provider_id, upstream_model, operation, surface,
         api_key_id, budget_group_id, attribution, model_family, estimate_provenance);
