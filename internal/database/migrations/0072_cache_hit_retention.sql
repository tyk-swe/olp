-- Cache replays count toward usage reports, but never represent provider calls
-- for invoice reconciliation. Retain their provider-scope request count.
ALTER TABLE olp.attempt_usage_hourly
    ADD COLUMN provider_cache_hit_count bigint NOT NULL DEFAULT 0
    CHECK (provider_cache_hit_count >= 0 AND provider_cache_hit_count <= provider_request_count);
