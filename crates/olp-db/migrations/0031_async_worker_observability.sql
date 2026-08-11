-- Replicated workers share task-level checkpoints. The closed task set keeps
-- observability cardinality bounded while allowing any replica to advance a
-- heartbeat or cumulative outcome counter.
CREATE TABLE worker_task_health (
    task text PRIMARY KEY CHECK (task IN (
        'runtime_outbox',
        'request_metadata_consumer',
        'maintenance',
        'request_metadata_gateway_epoch_detection'
    )),
    checked_at timestamptz NOT NULL,
    last_success_at timestamptz,
    last_progress_at timestamptz,
    successes_total bigint NOT NULL DEFAULT 0 CHECK (successes_total >= 0),
    failures_total bigint NOT NULL DEFAULT 0 CHECK (failures_total >= 0),
    skipped_total bigint NOT NULL DEFAULT 0 CHECK (skipped_total >= 0)
);

-- Cumulative recovery activity is kept separately from replaceable backlog
-- snapshots. In particular, recording an explicit Stream-loss incident may
-- remove the consumer-health row without resetting these counters.
CREATE TABLE async_worker_counters (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    request_metadata_reclaimed_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_reclaimed_total >= 0),
    request_metadata_recovered_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_recovered_total >= 0),
    request_metadata_duplicates_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_duplicates_total >= 0),
    request_metadata_processed_total bigint NOT NULL DEFAULT 0
        CHECK (request_metadata_processed_total >= 0),
    runtime_outbox_attempts_total bigint NOT NULL DEFAULT 0
        CHECK (runtime_outbox_attempts_total >= 0),
    runtime_outbox_retry_scheduled_total bigint NOT NULL DEFAULT 0
        CHECK (runtime_outbox_retry_scheduled_total >= 0),
    runtime_outbox_repeated_attempts_total bigint NOT NULL DEFAULT 0
        CHECK (runtime_outbox_repeated_attempts_total >= 0),
    runtime_outbox_published_total bigint NOT NULL DEFAULT 0
        CHECK (runtime_outbox_published_total >= 0),
    runtime_outbox_duplicate_publications_total bigint NOT NULL DEFAULT 0
        CHECK (runtime_outbox_duplicate_publications_total >= 0),
    runtime_outbox_abandoned_ownership_total bigint NOT NULL DEFAULT 0
        CHECK (runtime_outbox_abandoned_ownership_total >= 0),
    runtime_outbox_abandoned_claims_total bigint NOT NULL DEFAULT 0
        CHECK (runtime_outbox_abandoned_claims_total >= 0),
    runtime_outbox_failed_takeovers_total bigint NOT NULL DEFAULT 0
        CHECK (runtime_outbox_failed_takeovers_total >= 0)
);

INSERT INTO async_worker_counters (singleton) VALUES (true);

-- Advisory-lock ownership remains authoritative. This row is only a durable
-- summary: a surviving replica can identify an owner that disappeared before
-- clearing its claim, and a control/gateway process can report that state.
CREATE TABLE runtime_outbox_health (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    owner_active boolean NOT NULL,
    claimed_rows bigint NOT NULL DEFAULT 0 CHECK (claimed_rows BETWEEN 0 AND 1),
    checked_at timestamptz NOT NULL,
    last_progress_at timestamptz,
    CHECK (owner_active OR claimed_rows = 0)
);

-- A row-local attempt count makes publication after an ambiguous outcome
-- distinguishable from an ordinary first attempt. Published rows retain this
-- evidence until the existing seven-day outbox retention pass removes them.
ALTER TABLE transactional_outbox
    ADD COLUMN publication_attempts bigint NOT NULL DEFAULT 0
        CHECK (publication_attempts >= 0);
