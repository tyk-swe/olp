-- no-transaction
-- Provider health reads one bounded `started_at` window per provider. Without
-- this index the join scans every attempt ever recorded, for every page. See
-- 0040 for why each build is its own migration.
CREATE INDEX CONCURRENTLY attempts_provider_started_idx
    ON attempts(provider_id, started_at DESC);
