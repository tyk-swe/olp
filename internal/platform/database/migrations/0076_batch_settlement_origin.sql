ALTER TABLE olp.requests
    DROP CONSTRAINT requests_origin_check,
    DROP CONSTRAINT requests_origin_parent_check,
    ADD CONSTRAINT requests_origin_check
        CHECK (origin IN ('caller', 'shadow', 'classifier', 'probe', 'playground',
                          'cache_embedding', 'batch_settlement')),
    ADD CONSTRAINT requests_origin_parent_check
        CHECK ((parent_request_id IS NOT NULL) =
               (origin IN ('shadow', 'classifier', 'cache_embedding', 'batch_settlement')
                OR evaluator_guardrail_id IS NOT NULL));
