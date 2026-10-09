-- A console member's Playground call spends from the route's project and the
-- installation like a probe does, so it records its usage without an API key.
ALTER TABLE olp.requests
    DROP CONSTRAINT requests_origin_check,
    DROP CONSTRAINT requests_origin_key_check,
    ADD CONSTRAINT requests_origin_check
        CHECK (origin IN ('caller','shadow','classifier','probe','playground')),
    ADD CONSTRAINT requests_origin_key_check
        CHECK ((api_key_id IS NULL) = (origin IN ('shadow','probe','playground')));
