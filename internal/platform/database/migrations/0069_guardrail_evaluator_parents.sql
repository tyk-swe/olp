-- Model guardrails run as child requests, including keyless playground calls.
-- Match the accounting event contract while preserving ordinary origin rules.
ALTER TABLE olp.requests
    DROP CONSTRAINT requests_origin_parent_check,
    ADD CONSTRAINT requests_origin_parent_check
        CHECK ((parent_request_id IS NOT NULL) =
               (origin IN ('shadow','classifier') OR evaluator_guardrail_id IS NOT NULL));
