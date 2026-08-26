-- no-transaction
-- Resource-scoped audit filters: `resource_type` alone, or a `resource_type` and
-- `resource_id` pair. See 0040 for why each build is its own migration.
CREATE INDEX CONCURRENTLY audit_events_resource_occurred_idx
    ON audit_events(resource_type, resource_id, occurred_at DESC, id DESC);
