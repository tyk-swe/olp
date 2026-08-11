-- Durable worker-side visibility for the Valkey usage stream. Gateway-local
-- buffer metrics cannot reveal a poison entry or a stopped persistence
-- consumer, so the worker checkpoints group backlog and a heartbeat here.
CREATE TABLE usage_consumer_health (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    pending_events bigint NOT NULL CHECK (pending_events >= 0),
    lag_events bigint NOT NULL CHECK (lag_events >= 0),
    oldest_pending_at timestamptz,
    checked_at timestamptz NOT NULL
);
