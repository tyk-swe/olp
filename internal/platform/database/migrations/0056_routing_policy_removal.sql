-- A default-policy tombstone preserves a fresh installation ETag after removal.
-- A previously observed default cannot become current again after an intervening
-- create/delete cycle.
ALTER TABLE olp.routing_policies
    ADD COLUMN configured boolean NOT NULL DEFAULT true;
ALTER TABLE olp.routing_policies
    ADD CONSTRAINT routing_policy_tombstone_default
    CHECK (configured OR policy = '{}'::jsonb);
