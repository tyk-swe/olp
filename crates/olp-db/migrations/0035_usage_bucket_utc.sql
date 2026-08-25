-- Hour buckets are UTC hours everywhere in the reader path, but
-- `date_trunc('hour', <timestamptz>)` truncates in the *session* TimeZone. On
-- a server or role defaulting to a half-hour offset the rollup wrote (and this
-- constraint accepted) buckets that no UTC-derived query can ever match.
-- Restate the boundary against UTC and re-bucket rows already stamped with a
-- local-time hour, which would otherwise fail the new constraint.

-- The aggregate is fenced to the additive maintenance writer; this repair is
-- that same additive rewrite, performed once.
SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true);

CREATE TEMP TABLE request_metadata_gap_hourly_utc ON COMMIT DROP AS
SELECT date_trunc('hour', first_observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS bucket,
       gateway_instance,
       reason,
       SUM(event_count) AS event_count,
       SUM(uncertain_gap_count) AS uncertain_gap_count,
       MIN(first_observed_at) AS first_observed_at,
       MAX(last_observed_at) AS last_observed_at
  FROM request_metadata_gap_hourly
 GROUP BY 1, gateway_instance, reason;

DELETE FROM request_metadata_gap_hourly;

INSERT INTO request_metadata_gap_hourly
    (bucket, gateway_instance, reason, event_count, uncertain_gap_count,
     first_observed_at, last_observed_at)
SELECT bucket, gateway_instance, reason, event_count, uncertain_gap_count,
       first_observed_at, last_observed_at
  FROM request_metadata_gap_hourly_utc;

-- The original constraint was declared inline and carries a generated name.
DO $$
DECLARE
    stale_constraint text;
BEGIN
    FOR stale_constraint IN
        SELECT conname
          FROM pg_constraint
         WHERE conrelid = 'request_metadata_gap_hourly'::regclass
           AND contype = 'c'
           AND pg_get_constraintdef(oid) LIKE '%date_trunc%'
    LOOP
        EXECUTE format(
            'ALTER TABLE request_metadata_gap_hourly DROP CONSTRAINT %I', stale_constraint);
    END LOOP;
END $$;

ALTER TABLE request_metadata_gap_hourly
    ADD CONSTRAINT request_metadata_gap_hourly_bucket_utc_check
    CHECK (bucket = date_trunc('hour', first_observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC');
