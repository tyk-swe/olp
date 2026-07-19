-- Raw usage facts age into retained hourly aggregates. Preserve the matching
-- completeness evidence as a normalized, metadata-only hourly rollup so an
-- old outage can never become falsely complete after raw gap retention.
CREATE TABLE usage_gap_hourly (
    bucket timestamptz NOT NULL,
    gateway_instance text NOT NULL,
    reason text NOT NULL,
    event_count bigint NOT NULL CHECK (event_count > 0),
    first_observed_at timestamptz NOT NULL,
    last_observed_at timestamptz NOT NULL,
    CHECK (bucket = date_trunc('hour', first_observed_at)),
    CHECK (last_observed_at >= first_observed_at),
    PRIMARY KEY (bucket, gateway_instance, reason)
);

CREATE INDEX usage_gap_hourly_overlap_idx
    ON usage_gap_hourly (last_observed_at, first_observed_at);
