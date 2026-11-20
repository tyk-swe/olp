-- Response bodies live only in the dedicated, sealed cache. These columns are
-- metadata for requests and their zero-provider-cost cache attempts.
ALTER TABLE olp.requests ADD COLUMN cache_result text
    CHECK (cache_result IN ('hit','semantic-hit','miss','bypass'));
ALTER TABLE olp.attempt_usage_facts
    ADD COLUMN response_cache boolean NOT NULL DEFAULT false,
    ADD COLUMN cache_avoided_cost numeric(24,12) NOT NULL DEFAULT 0 CHECK (cache_avoided_cost >= 0),
    ADD COLUMN cache_budget_cost numeric(24,12) NOT NULL DEFAULT 0 CHECK (cache_budget_cost >= 0),
    ADD COLUMN cache_budget_charge numeric(24,12) NOT NULL DEFAULT 0 CHECK (cache_budget_charge >= 0);
CREATE INDEX requests_cache_result_idx ON olp.requests(started_at,cache_result) WHERE cache_result IS NOT NULL;

ALTER TABLE olp.requests
    DROP CONSTRAINT requests_origin_check,
    DROP CONSTRAINT requests_origin_parent_check,
    ADD CONSTRAINT requests_origin_check
        CHECK (origin IN ('caller','shadow','classifier','probe','playground','cache_embedding')),
    ADD CONSTRAINT requests_origin_parent_check
        CHECK ((parent_request_id IS NOT NULL) =
               (origin IN ('shadow','classifier','cache_embedding') OR evaluator_guardrail_id IS NOT NULL));
