-- `attempt_usage_facts.attempt_started_at` and `.attempt_completed_at` were
-- meant to carry per-attempt latency attribution. Nothing ever read them: the
-- request explorer serves the attempt window from `attempts.started_at` and
-- `attempts.completed_at`, and the hourly rollup never carried either column.
--
-- Only the NOT NULL goes now. The ingestion INSERT stops naming both columns in
-- this release, so they have to accept NULL; the 2.0.1 binary still supplies
-- them, so they cannot be dropped until no 2.0.1 replica is left to write them.
-- The columns themselves go in a follow-up release.
--
-- The rolling-upgrade mirror trigger writes them, so replace it first. Its
-- remaining behaviour is unchanged.
CREATE OR REPLACE FUNCTION mirror_legacy_usage_fact_to_attempt() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM attempt_usage_facts WHERE request_id = NEW.request_id) THEN
        RETURN NEW;
    END IF;

    WITH retained AS (
        SELECT attempt.id AS attempt_id, attempt.ordinal, attempt.provider_id,
               attempt.upstream_model, attempt.started_at, attempt.completed_at,
               attempt.committed, attempt.error_class, false AS synthesized
          FROM attempts attempt
         WHERE attempt.request_id = NEW.request_id
           AND attempt.request_started_at = NEW.request_started_at
        UNION ALL
        SELECT NEW.id, 1::smallint, NEW.provider_id, NEW.upstream_model,
               NEW.request_started_at, NEW.observed_at, true, NULL::text, true
         WHERE NOT EXISTS (
             SELECT 1 FROM attempts attempt
              WHERE attempt.request_id = NEW.request_id
                AND attempt.request_started_at = NEW.request_started_at
         )
    ), located AS (
        SELECT retained.*,
               max(ordinal) FILTER (
                   WHERE provider_id = NEW.provider_id
                     AND upstream_model = NEW.upstream_model
               ) OVER () AS matching_ordinal,
               max(ordinal) OVER () AS final_ordinal,
               (NEW.usage_complete OR NEW.input_tokens IS NOT NULL OR
                NEW.output_tokens IS NOT NULL OR NEW.cached_input_tokens IS NOT NULL OR
                NEW.media_units IS NOT NULL) AS legacy_usage_observed
          FROM retained
    ), attributed AS (
        SELECT located.*,
               ordinal = COALESCE(matching_ordinal, final_ordinal, 1::smallint)
                   AS carries_legacy_usage
          FROM located
    ), classified AS (
        SELECT attributed.*,
               CASE
                 WHEN carries_legacy_usage AND legacy_usage_observed
                   THEN 'billable'::attempt_charge_status
                 WHEN committed OR synthesized OR error_class IN
                      ('ambiguous', 'timeout', 'upstream_server', 'protocol', 'cancelled')
                   THEN 'billing_uncertain'::attempt_charge_status
                 ELSE 'not_billable'::attempt_charge_status
               END AS derived_charge_status
          FROM attributed
    ), marked AS (
        SELECT classified.*,
               row_number() OVER (ORDER BY ordinal) = 1 AS request_marker,
               row_number() OVER (PARTITION BY provider_id ORDER BY ordinal) = 1
                   AS provider_marker,
               row_number() OVER (PARTITION BY upstream_model ORDER BY ordinal) = 1
                   AS model_marker,
               row_number() OVER (
                   PARTITION BY provider_id, upstream_model ORDER BY ordinal
               ) = 1 AS target_marker,
               bool_or(derived_charge_status <> 'not_billable' AND
                       CASE WHEN carries_legacy_usage THEN NEW.unpriced ELSE true END)
                   OVER () AS request_unpriced,
               bool_or(derived_charge_status <> 'not_billable' AND
                       CASE WHEN carries_legacy_usage THEN NEW.unpriced ELSE true END)
                   OVER (PARTITION BY provider_id) AS provider_unpriced,
               bool_or(derived_charge_status <> 'not_billable' AND
                       CASE WHEN carries_legacy_usage THEN NEW.unpriced ELSE true END)
                   OVER (PARTITION BY upstream_model) AS model_unpriced,
               bool_or(derived_charge_status <> 'not_billable' AND
                       CASE WHEN carries_legacy_usage THEN NEW.unpriced ELSE true END)
                   OVER (PARTITION BY provider_id, upstream_model) AS target_unpriced,
               bool_or(derived_charge_status <> 'not_billable' AND
                       NOT (carries_legacy_usage AND NEW.usage_complete))
                   OVER () AS request_incomplete,
               bool_or(derived_charge_status <> 'not_billable' AND
                       NOT (carries_legacy_usage AND NEW.usage_complete))
                   OVER (PARTITION BY provider_id) AS provider_incomplete,
               bool_or(derived_charge_status <> 'not_billable' AND
                       NOT (carries_legacy_usage AND NEW.usage_complete))
                   OVER (PARTITION BY upstream_model) AS model_incomplete,
               bool_or(derived_charge_status <> 'not_billable' AND
                       NOT (carries_legacy_usage AND NEW.usage_complete))
                   OVER (PARTITION BY provider_id, upstream_model) AS target_incomplete
          FROM classified
    )
    INSERT INTO attempt_usage_facts (
        attempt_id, event_id, request_id, request_started_at, attempt_ordinal,
        api_key_id, provider_id, route_slug, upstream_model, operation, surface,
        observed_at, charge_status,
        usage_observed, usage_complete, input_tokens, output_tokens,
        cached_input_tokens, media_units, estimated_cost, unpriced,
        pricing_revision_id, currency, request_counted, provider_request_counted,
        model_request_counted, target_request_counted, request_unpriced_counted,
        provider_unpriced_counted, model_unpriced_counted, target_unpriced_counted,
        request_incomplete_counted, provider_incomplete_counted,
        model_incomplete_counted, target_incomplete_counted
    )
    SELECT attempt_id, NEW.id, NEW.request_id, NEW.request_started_at, ordinal,
           NEW.api_key_id, provider_id, NEW.route_slug, upstream_model, NEW.operation,
           NEW.surface, NEW.observed_at,
           derived_charge_status,
           carries_legacy_usage AND legacy_usage_observed,
           CASE WHEN derived_charge_status = 'not_billable' THEN true
                WHEN carries_legacy_usage THEN NEW.usage_complete ELSE false END,
           CASE WHEN carries_legacy_usage THEN NEW.input_tokens END,
           CASE WHEN carries_legacy_usage THEN NEW.output_tokens END,
           CASE WHEN carries_legacy_usage THEN NEW.cached_input_tokens END,
           CASE WHEN carries_legacy_usage THEN NEW.media_units END,
           CASE WHEN carries_legacy_usage AND NEW.usage_complete
                     AND NOT NEW.unpriced THEN NEW.estimated_cost END,
           CASE WHEN derived_charge_status = 'not_billable' THEN false
                WHEN carries_legacy_usage THEN NEW.unpriced ELSE true END,
           CASE WHEN carries_legacy_usage AND derived_charge_status <> 'not_billable'
                THEN NEW.pricing_revision_id END,
           CASE WHEN carries_legacy_usage AND derived_charge_status <> 'not_billable'
                THEN NEW.currency END,
           request_marker, provider_marker, model_marker, target_marker,
           request_marker AND request_unpriced,
           provider_marker AND provider_unpriced,
           model_marker AND model_unpriced,
           target_marker AND target_unpriced,
           request_marker AND request_incomplete,
           provider_marker AND provider_incomplete,
           model_marker AND model_incomplete,
           target_marker AND target_incomplete
      FROM marked
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- The `attempt_completed_at >= attempt_started_at` CHECK stays: it holds
-- vacuously once either column is NULL, and still orders the windows that a
-- 2.0.1 replica writes.
ALTER TABLE attempt_usage_facts
    ALTER COLUMN attempt_started_at DROP NOT NULL,
    ALTER COLUMN attempt_completed_at DROP NOT NULL;
