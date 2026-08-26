-- no-transaction
-- "What did this operator do" - the actor-scoped audit filter. See 0040 for why
-- each build is its own migration.
CREATE INDEX CONCURRENTLY audit_events_actor_occurred_idx
    ON audit_events(actor_user_id, occurred_at DESC, id DESC);
