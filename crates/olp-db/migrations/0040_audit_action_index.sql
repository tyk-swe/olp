-- no-transaction
-- The audit page now filters on action, resource, and actor, and always orders
-- by `(occurred_at DESC, id DESC)`. `audit_events_occurred_at_idx` only serves
-- the unfiltered page; a filtered one degrades into a scan of the whole stream.
-- Each of the next three indexes leads with its filter column and closes on the
-- sort key, so a filtered page is one index range read in cursor order.
--
-- Built CONCURRENTLY - and therefore outside a transaction, like 0034 - because
-- `audit_events` is append-only and large, and a gateway must keep writing it
-- while the migration runs. PostgreSQL wraps a multi-statement simple query in
-- an implicit transaction, so each concurrent build gets its own migration.
CREATE INDEX CONCURRENTLY audit_events_action_occurred_idx
    ON audit_events(action, occurred_at DESC, id DESC);
