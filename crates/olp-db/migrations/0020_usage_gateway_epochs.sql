-- A gateway writes this metadata-only lease from its existing background
-- usage reporter. A separate table preserves rolling compatibility with the
-- pre-epoch cumulative-loss checkpoint while retaining every new process
-- epoch for abrupt-stop detection.
CREATE TABLE usage_gateway_epochs (
    gateway_instance text NOT NULL,
    process_epoch uuid NOT NULL,
    started_at timestamptz NOT NULL,
    accepted bigint NOT NULL CHECK (accepted >= 0),
    persisted bigint NOT NULL CHECK (persisted >= 0),
    dropped bigint NOT NULL CHECK (dropped >= 0),
    abandoned bigint NOT NULL CHECK (abandoned >= 0),
    retrying boolean NOT NULL,
    writer_closed boolean NOT NULL,
    updated_at timestamptz NOT NULL,
    gracefully_closed_at timestamptz,
    stale_candidate_at timestamptz,
    stale_detected_at timestamptz,
    acknowledged_at timestamptz,
    acknowledged_by uuid REFERENCES users(id) ON DELETE SET NULL,
    uncertainty_gap_id uuid REFERENCES usage_ingestion_gaps(id) ON DELETE SET NULL,
    PRIMARY KEY (gateway_instance, process_epoch),
    CHECK (updated_at >= started_at),
    CHECK (gracefully_closed_at IS NULL OR gracefully_closed_at >= started_at),
    CHECK (stale_candidate_at IS NULL OR stale_candidate_at >= started_at),
    CHECK (stale_detected_at IS NULL OR stale_detected_at >= started_at),
    CHECK (acknowledged_at IS NULL OR (
        stale_detected_at IS NOT NULL AND acknowledged_at >= stale_detected_at
    )),
    CHECK (NOT (gracefully_closed_at IS NOT NULL AND stale_detected_at IS NOT NULL))
);

CREATE UNIQUE INDEX usage_gateway_epochs_one_open_idx
    ON usage_gateway_epochs (gateway_instance)
    WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL;

-- Process epochs are generated once per process with UUIDv7 and form the
-- stable management identifier used by the acknowledgement endpoint.
CREATE UNIQUE INDEX usage_gateway_epochs_process_epoch_idx
    ON usage_gateway_epochs (process_epoch);

CREATE INDEX usage_gateway_epochs_stale_scan_idx
    ON usage_gateway_epochs (updated_at)
    WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL;

CREATE INDEX usage_gateway_epochs_unresolved_idx
    ON usage_gateway_epochs (stale_detected_at)
    WHERE stale_detected_at IS NOT NULL AND acknowledged_at IS NULL;

-- Exact local drops and uncertainty caused by an unclean epoch are distinct.
-- A zero lower bound is valid: the absence of a final checkpoint still makes
-- completeness unknown even when the last checkpoint had no queued events.
CREATE TYPE usage_gap_certainty AS ENUM ('exact', 'lower_bound');

ALTER TABLE usage_ingestion_gaps
    DROP CONSTRAINT usage_ingestion_gaps_event_count_check,
    ADD COLUMN certainty usage_gap_certainty NOT NULL DEFAULT 'exact',
    ADD CHECK (event_count >= 0),
    ADD CHECK (certainty = 'lower_bound' OR event_count > 0);

ALTER TABLE usage_gap_hourly
    DROP CONSTRAINT usage_gap_hourly_event_count_check,
    ADD COLUMN uncertain_gap_count bigint NOT NULL DEFAULT 0
        CHECK (uncertain_gap_count >= 0),
    ADD CHECK (event_count >= 0),
    ADD CHECK (event_count > 0 OR uncertain_gap_count > 0);
