ALTER TABLE olp.requests ADD COLUMN cache_lookup_reason text
    CHECK (cache_lookup_reason IS NULL
        OR (cache_lookup_reason = 'refresh' AND cache_result IS NOT DISTINCT FROM 'miss')
        OR (cache_lookup_reason IN ('no_store', 'multiple_choices', 'backend_unavailable')
            AND cache_result IS NOT DISTINCT FROM 'bypass'));
