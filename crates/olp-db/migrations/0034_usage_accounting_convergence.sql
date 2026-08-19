-- Converge usage accounting on the canonical attempt-level model.
-- Reconcile any remaining unreconciled records from legacy usage tables,
-- assert strict pre- and post-reconciliation parity across all dimensions,
-- drop legacy mirror/guard triggers and functions, and retire legacy tables.

DO $reconciliation$
DECLARE
    v_pre_attempt_requests bigint;
    v_pre_attempt_input_tokens numeric(30, 0);
    v_pre_attempt_output_tokens numeric(30, 0);
    v_pre_attempt_cached_tokens numeric(30, 0);
    v_pre_attempt_media_units numeric(30, 6);
    v_pre_attempt_cost numeric(30, 12);
    v_pre_attempt_unpriced bigint;
    v_pre_attempt_incomplete bigint;

    v_reconciled_facts_requests bigint;
    v_reconciled_facts_input_tokens numeric(30, 0);
    v_reconciled_facts_output_tokens numeric(30, 0);
    v_reconciled_facts_cached_tokens numeric(30, 0);
    v_reconciled_facts_media_units numeric(30, 6);
    v_reconciled_facts_cost numeric(30, 12);
    v_reconciled_facts_unpriced bigint;
    v_reconciled_facts_incomplete bigint;

    v_reconciled_hourly_requests bigint;
    v_reconciled_hourly_input_tokens numeric(30, 0);
    v_reconciled_hourly_output_tokens numeric(30, 0);
    v_reconciled_hourly_cached_tokens numeric(30, 0);
    v_reconciled_hourly_media_units numeric(30, 6);
    v_reconciled_hourly_cost numeric(30, 12);
    v_reconciled_hourly_unpriced bigint;
    v_reconciled_hourly_incomplete bigint;

    v_post_attempt_requests bigint;
    v_post_attempt_input_tokens numeric(30, 0);
    v_post_attempt_output_tokens numeric(30, 0);
    v_post_attempt_cached_tokens numeric(30, 0);
    v_post_attempt_media_units numeric(30, 6);
    v_post_attempt_cost numeric(30, 12);
    v_post_attempt_unpriced bigint;
    v_post_attempt_incomplete bigint;

    v_expected_requests bigint;
    v_expected_input_tokens numeric(30, 0);
    v_expected_output_tokens numeric(30, 0);
    v_expected_cached_tokens numeric(30, 0);
    v_expected_media_units numeric(30, 6);
    v_expected_cost numeric(30, 12);
    v_expected_unpriced bigint;
    v_expected_incomplete bigint;

    v_missing_legacy_fact_count bigint;
BEGIN
    -- Enable additive rollup writer fence for attempt_usage_hourly inserts
    PERFORM set_config('olp.usage_rollup_writer', 'additive-v2', true);

    -- 1. Snapshot pre-reconciliation attempt totals (facts + hourly)
    SELECT
        COALESCE(SUM(fact_req), 0) + COALESCE(SUM(hourly_req), 0),
        COALESCE(SUM(fact_in), 0) + COALESCE(SUM(hourly_in), 0),
        COALESCE(SUM(fact_out), 0) + COALESCE(SUM(hourly_out), 0),
        COALESCE(SUM(fact_cached), 0) + COALESCE(SUM(hourly_cached), 0),
        COALESCE(SUM(fact_media), 0) + COALESCE(SUM(hourly_media), 0),
        COALESCE(SUM(fact_cost), 0) + COALESCE(SUM(hourly_cost), 0),
        COALESCE(SUM(fact_unpriced), 0) + COALESCE(SUM(hourly_unpriced), 0),
        COALESCE(SUM(fact_incomplete), 0) + COALESCE(SUM(hourly_incomplete), 0)
    INTO
        v_pre_attempt_requests,
        v_pre_attempt_input_tokens,
        v_pre_attempt_output_tokens,
        v_pre_attempt_cached_tokens,
        v_pre_attempt_media_units,
        v_pre_attempt_cost,
        v_pre_attempt_unpriced,
        v_pre_attempt_incomplete
    FROM (
        SELECT
            COUNT(*) FILTER (WHERE request_counted) AS fact_req,
            COALESCE(SUM(input_tokens), 0) AS fact_in,
            COALESCE(SUM(output_tokens), 0) AS fact_out,
            COALESCE(SUM(cached_input_tokens), 0) AS fact_cached,
            COALESCE(SUM(media_units), 0) AS fact_media,
            COALESCE(SUM(estimated_cost), 0) AS fact_cost,
            COUNT(*) FILTER (WHERE request_unpriced_counted) AS fact_unpriced,
            COUNT(*) FILTER (WHERE request_incomplete_counted) AS fact_incomplete,
            0::bigint AS hourly_req,
            0::numeric(30, 0) AS hourly_in,
            0::numeric(30, 0) AS hourly_out,
            0::numeric(30, 0) AS hourly_cached,
            0::numeric(30, 6) AS hourly_media,
            0::numeric(30, 12) AS hourly_cost,
            0::bigint AS hourly_unpriced,
            0::bigint AS hourly_incomplete
        FROM attempt_usage_facts
        UNION ALL
        SELECT
            0, 0, 0, 0, 0, 0, 0, 0,
            COALESCE(SUM(request_count), 0),
            COALESCE(SUM(input_tokens), 0),
            COALESCE(SUM(output_tokens), 0),
            COALESCE(SUM(cached_input_tokens), 0),
            COALESCE(SUM(media_units), 0),
            COALESCE(SUM(estimated_cost), 0),
            COALESCE(SUM(request_unpriced_count), 0),
            COALESCE(SUM(request_incomplete_count), 0)
        FROM attempt_usage_hourly
    ) pre;

    -- 2. Identify unreconciled legacy facts
    SELECT
        COUNT(*),
        COALESCE(SUM(input_tokens), 0),
        COALESCE(SUM(output_tokens), 0),
        COALESCE(SUM(cached_input_tokens), 0),
        COALESCE(SUM(media_units), 0),
        COALESCE(SUM(estimated_cost), 0)
    INTO
        v_reconciled_facts_requests,
        v_reconciled_facts_input_tokens,
        v_reconciled_facts_output_tokens,
        v_reconciled_facts_cached_tokens,
        v_reconciled_facts_media_units,
        v_reconciled_facts_cost
    FROM usage_facts fact
    WHERE NOT EXISTS (
        SELECT 1 FROM attempt_usage_facts a WHERE a.request_id = fact.request_id
    );

    v_reconciled_facts_unpriced := 0;
    v_reconciled_facts_incomplete := 0;

    -- 3. Reconcile any unreconciled legacy facts into attempt_usage_facts
    IF v_reconciled_facts_requests > 0 THEN
        WITH legacy AS (
            SELECT fact.*,
                   attempt.id AS retained_attempt_id,
                   COALESCE(attempt.ordinal, 1::smallint) AS retained_ordinal,
                   COALESCE(attempt.started_at, fact.request_started_at) AS retained_started_at,
                   COALESCE(attempt.completed_at, fact.observed_at) AS retained_completed_at,
                   COALESCE(attempt.provider_id, fact.provider_id) AS retained_provider_id,
                   COALESCE(attempt.upstream_model, fact.upstream_model) AS retained_model,
                   COALESCE(attempt.committed, true) AS retained_committed,
                   attempt.error_class AS retained_error_class,
                   max(attempt.ordinal) FILTER (
                       WHERE attempt.provider_id = fact.provider_id
                         AND attempt.upstream_model = fact.upstream_model
                   ) OVER (PARTITION BY fact.request_id) AS matching_ordinal,
                   max(attempt.ordinal) OVER (PARTITION BY fact.request_id) AS final_ordinal
              FROM usage_facts fact
              LEFT JOIN attempts attempt
                ON attempt.request_id = fact.request_id
               AND attempt.request_started_at = fact.request_started_at
             WHERE NOT EXISTS (
                 SELECT 1 FROM attempt_usage_facts a WHERE a.request_id = fact.request_id
             )
        ), classified AS (
            SELECT legacy.*,
                   retained_ordinal = COALESCE(matching_ordinal, final_ordinal, 1::smallint)
                       AS carries_legacy_usage,
                   CASE
                     WHEN retained_ordinal = COALESCE(matching_ordinal, final_ordinal, 1::smallint)
                          AND (usage_complete OR input_tokens IS NOT NULL OR output_tokens IS NOT NULL
                               OR cached_input_tokens IS NOT NULL OR media_units IS NOT NULL)
                       THEN 'billable'::attempt_charge_status
                     WHEN retained_committed OR retained_error_class IN
                          ('ambiguous', 'timeout', 'upstream_server', 'protocol', 'cancelled')
                       THEN 'billing_uncertain'::attempt_charge_status
                     ELSE 'not_billable'::attempt_charge_status
                   END AS derived_charge_status
              FROM legacy
        ), marked AS (
            SELECT classified.*,
                   row_number() OVER (PARTITION BY request_id ORDER BY retained_ordinal) = 1
                       AS request_marker,
                   row_number() OVER (
                       PARTITION BY request_id, retained_provider_id ORDER BY retained_ordinal
                   ) = 1 AS provider_marker,
                   row_number() OVER (
                       PARTITION BY request_id, retained_model ORDER BY retained_ordinal
                   ) = 1 AS model_marker,
                   row_number() OVER (
                       PARTITION BY request_id, retained_provider_id, retained_model
                       ORDER BY retained_ordinal
                   ) = 1 AS target_marker,
                   bool_or(derived_charge_status <> 'not_billable' AND
                           CASE WHEN carries_legacy_usage THEN unpriced ELSE true END)
                       OVER (PARTITION BY request_id) AS request_unpriced,
                   bool_or(derived_charge_status <> 'not_billable' AND
                           CASE WHEN carries_legacy_usage THEN unpriced ELSE true END)
                       OVER (PARTITION BY request_id, retained_provider_id) AS provider_unpriced,
                   bool_or(derived_charge_status <> 'not_billable' AND
                           CASE WHEN carries_legacy_usage THEN unpriced ELSE true END)
                       OVER (PARTITION BY request_id, retained_model) AS model_unpriced,
                   bool_or(derived_charge_status <> 'not_billable' AND
                           CASE WHEN carries_legacy_usage THEN unpriced ELSE true END)
                       OVER (PARTITION BY request_id, retained_provider_id, retained_model)
                       AS target_unpriced,
                   bool_or(derived_charge_status <> 'not_billable' AND
                           NOT (carries_legacy_usage AND usage_complete))
                       OVER (PARTITION BY request_id) AS request_incomplete,
                   bool_or(derived_charge_status <> 'not_billable' AND
                           NOT (carries_legacy_usage AND usage_complete))
                       OVER (PARTITION BY request_id, retained_provider_id) AS provider_incomplete,
                   bool_or(derived_charge_status <> 'not_billable' AND
                           NOT (carries_legacy_usage AND usage_complete))
                       OVER (PARTITION BY request_id, retained_model) AS model_incomplete,
                   bool_or(derived_charge_status <> 'not_billable' AND
                           NOT (carries_legacy_usage AND usage_complete))
                       OVER (PARTITION BY request_id, retained_provider_id, retained_model)
                       AS target_incomplete
              FROM classified
        ), inserted AS (
            INSERT INTO attempt_usage_facts (
                attempt_id, event_id, request_id, request_started_at, attempt_ordinal,
                api_key_id, provider_id, route_slug, upstream_model, operation, surface,
                attempt_started_at, attempt_completed_at, observed_at, charge_status,
                usage_observed, usage_complete, input_tokens, output_tokens,
                cached_input_tokens, media_units, estimated_cost, unpriced,
                pricing_revision_id, currency, request_counted, provider_request_counted,
                model_request_counted, target_request_counted, request_unpriced_counted,
                provider_unpriced_counted, model_unpriced_counted, target_unpriced_counted,
                request_incomplete_counted, provider_incomplete_counted,
                model_incomplete_counted, target_incomplete_counted
            )
            SELECT COALESCE(retained_attempt_id, id), id, request_id, request_started_at,
                   retained_ordinal, api_key_id, retained_provider_id, route_slug,
                   retained_model, operation, surface, retained_started_at,
                   GREATEST(retained_completed_at, retained_started_at), observed_at,
                   derived_charge_status,
                   carries_legacy_usage AND (usage_complete OR input_tokens IS NOT NULL OR
                       output_tokens IS NOT NULL OR cached_input_tokens IS NOT NULL OR media_units IS NOT NULL),
                   CASE WHEN derived_charge_status = 'not_billable' THEN true
                        WHEN carries_legacy_usage THEN usage_complete ELSE false END,
                   CASE WHEN carries_legacy_usage THEN input_tokens END,
                   CASE WHEN carries_legacy_usage THEN output_tokens END,
                   CASE WHEN carries_legacy_usage THEN cached_input_tokens END,
                   CASE WHEN carries_legacy_usage THEN media_units END,
                   CASE WHEN carries_legacy_usage AND usage_complete THEN estimated_cost END,
                   CASE WHEN derived_charge_status = 'not_billable' THEN false
                        WHEN carries_legacy_usage THEN unpriced ELSE true END,
                   CASE WHEN carries_legacy_usage THEN pricing_revision_id END,
                   CASE WHEN carries_legacy_usage THEN currency END,
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
            ON CONFLICT (request_id, attempt_ordinal) DO NOTHING
            RETURNING request_unpriced_counted, request_incomplete_counted
        )
        SELECT
            COUNT(*) FILTER (WHERE request_unpriced_counted),
            COUNT(*) FILTER (WHERE request_incomplete_counted)
        INTO
            v_reconciled_facts_unpriced,
            v_reconciled_facts_incomplete
        FROM inserted;
    END IF;

    -- 4. Reconcile any unreconciled legacy hourly rows into attempt_usage_hourly
    SELECT
        COALESCE(SUM(request_count), 0),
        COALESCE(SUM(input_tokens), 0),
        COALESCE(SUM(output_tokens), 0),
        COALESCE(SUM(cached_input_tokens), 0),
        COALESCE(SUM(media_units), 0),
        COALESCE(SUM(estimated_cost), 0),
        COALESCE(SUM(unpriced_count), 0),
        COALESCE(SUM(incomplete_count), 0)
    INTO
        v_reconciled_hourly_requests,
        v_reconciled_hourly_input_tokens,
        v_reconciled_hourly_output_tokens,
        v_reconciled_hourly_cached_tokens,
        v_reconciled_hourly_media_units,
        v_reconciled_hourly_cost,
        v_reconciled_hourly_unpriced,
        v_reconciled_hourly_incomplete
    FROM usage_hourly h
    WHERE NOT EXISTS (
        SELECT 1 FROM attempt_usage_hourly a
        WHERE a.bucket = h.bucket
          AND a.route_slug = h.route_slug
          AND a.provider_id = h.provider_id
          AND a.upstream_model = h.upstream_model
          AND a.operation = h.operation
          AND a.surface = h.surface
          AND a.api_key_id IS NOT DISTINCT FROM h.api_key_id
    );

    IF v_reconciled_hourly_requests > 0 THEN
        INSERT INTO attempt_usage_hourly (
            bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
            request_count, provider_request_count, model_request_count, target_request_count,
            input_tokens, output_tokens, cached_input_tokens, media_units, estimated_cost,
            request_unpriced_count, provider_unpriced_count, model_unpriced_count,
            target_unpriced_count, request_incomplete_count, provider_incomplete_count,
            model_incomplete_count, target_incomplete_count, currency
        )
        SELECT bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
               request_count, request_count, request_count, request_count,
               input_tokens, output_tokens, cached_input_tokens, media_units, estimated_cost,
               unpriced_count, unpriced_count, unpriced_count, unpriced_count,
               incomplete_count, incomplete_count, incomplete_count, incomplete_count, currency
          FROM usage_hourly h
         WHERE NOT EXISTS (
             SELECT 1 FROM attempt_usage_hourly a
             WHERE a.bucket = h.bucket
               AND a.route_slug = h.route_slug
               AND a.provider_id = h.provider_id
               AND a.upstream_model = h.upstream_model
               AND a.operation = h.operation
               AND a.surface = h.surface
               AND a.api_key_id IS NOT DISTINCT FROM h.api_key_id
         );
    END IF;

    -- 5. Check that every request in usage_facts is now represented in attempt_usage_facts
    SELECT COUNT(*) INTO v_missing_legacy_fact_count
      FROM usage_facts fact
     WHERE NOT EXISTS (
         SELECT 1 FROM attempt_usage_facts a WHERE a.request_id = fact.request_id
     );
    IF v_missing_legacy_fact_count > 0 THEN
        RAISE EXCEPTION 'usage accounting reconciliation failed: % legacy facts unrepresented in attempt_usage_facts',
            v_missing_legacy_fact_count USING ERRCODE = '55000';
    END IF;

    -- 6. Snapshot post-reconciliation attempt totals
    SELECT
        COALESCE(SUM(fact_req), 0) + COALESCE(SUM(hourly_req), 0),
        COALESCE(SUM(fact_in), 0) + COALESCE(SUM(hourly_in), 0),
        COALESCE(SUM(fact_out), 0) + COALESCE(SUM(hourly_out), 0),
        COALESCE(SUM(fact_cached), 0) + COALESCE(SUM(hourly_cached), 0),
        COALESCE(SUM(fact_media), 0) + COALESCE(SUM(hourly_media), 0),
        COALESCE(SUM(fact_cost), 0) + COALESCE(SUM(hourly_cost), 0),
        COALESCE(SUM(fact_unpriced), 0) + COALESCE(SUM(hourly_unpriced), 0),
        COALESCE(SUM(fact_incomplete), 0) + COALESCE(SUM(hourly_incomplete), 0)
    INTO
        v_post_attempt_requests,
        v_post_attempt_input_tokens,
        v_post_attempt_output_tokens,
        v_post_attempt_cached_tokens,
        v_post_attempt_media_units,
        v_post_attempt_cost,
        v_post_attempt_unpriced,
        v_post_attempt_incomplete
    FROM (
        SELECT
            COUNT(*) FILTER (WHERE request_counted) AS fact_req,
            COALESCE(SUM(input_tokens), 0) AS fact_in,
            COALESCE(SUM(output_tokens), 0) AS fact_out,
            COALESCE(SUM(cached_input_tokens), 0) AS fact_cached,
            COALESCE(SUM(media_units), 0) AS fact_media,
            COALESCE(SUM(estimated_cost), 0) AS fact_cost,
            COUNT(*) FILTER (WHERE request_unpriced_counted) AS fact_unpriced,
            COUNT(*) FILTER (WHERE request_incomplete_counted) AS fact_incomplete,
            0::bigint AS hourly_req,
            0::numeric(30, 0) AS hourly_in,
            0::numeric(30, 0) AS hourly_out,
            0::numeric(30, 0) AS hourly_cached,
            0::numeric(30, 6) AS hourly_media,
            0::numeric(30, 12) AS hourly_cost,
            0::bigint AS hourly_unpriced,
            0::bigint AS hourly_incomplete
        FROM attempt_usage_facts
        UNION ALL
        SELECT
            0, 0, 0, 0, 0, 0, 0, 0,
            COALESCE(SUM(request_count), 0),
            COALESCE(SUM(input_tokens), 0),
            COALESCE(SUM(output_tokens), 0),
            COALESCE(SUM(cached_input_tokens), 0),
            COALESCE(SUM(media_units), 0),
            COALESCE(SUM(estimated_cost), 0),
            COALESCE(SUM(request_unpriced_count), 0),
            COALESCE(SUM(request_incomplete_count), 0)
        FROM attempt_usage_hourly
    ) post;

    -- 7. Calculate expected post totals = pre_attempt + reconciled_legacy
    v_expected_requests := v_pre_attempt_requests + v_reconciled_facts_requests + v_reconciled_hourly_requests;
    v_expected_input_tokens := v_pre_attempt_input_tokens + v_reconciled_facts_input_tokens + v_reconciled_hourly_input_tokens;
    v_expected_output_tokens := v_pre_attempt_output_tokens + v_reconciled_facts_output_tokens + v_reconciled_hourly_output_tokens;
    v_expected_cached_tokens := v_pre_attempt_cached_tokens + v_reconciled_facts_cached_tokens + v_reconciled_hourly_cached_tokens;
    v_expected_media_units := v_pre_attempt_media_units + v_reconciled_facts_media_units + v_reconciled_hourly_media_units;
    v_expected_cost := v_pre_attempt_cost + v_reconciled_facts_cost + v_reconciled_hourly_cost;
    v_expected_unpriced := v_pre_attempt_unpriced + v_reconciled_facts_unpriced + v_reconciled_hourly_unpriced;
    v_expected_incomplete := v_pre_attempt_incomplete + v_reconciled_facts_incomplete + v_reconciled_hourly_incomplete;

    -- 8. Assert parity across all 8 dimensions
    IF v_post_attempt_requests <> v_expected_requests THEN
        RAISE EXCEPTION 'usage reconciliation parity mismatch: request_count expected %, got %',
            v_expected_requests, v_post_attempt_requests USING ERRCODE = '55000';
    END IF;

    IF v_post_attempt_input_tokens <> v_expected_input_tokens THEN
        RAISE EXCEPTION 'usage reconciliation parity mismatch: input_tokens expected %, got %',
            v_expected_input_tokens, v_post_attempt_input_tokens USING ERRCODE = '55000';
    END IF;

    IF v_post_attempt_output_tokens <> v_expected_output_tokens THEN
        RAISE EXCEPTION 'usage reconciliation parity mismatch: output_tokens expected %, got %',
            v_expected_output_tokens, v_post_attempt_output_tokens USING ERRCODE = '55000';
    END IF;

    IF v_post_attempt_cached_tokens <> v_expected_cached_tokens THEN
        RAISE EXCEPTION 'usage reconciliation parity mismatch: cached_input_tokens expected %, got %',
            v_expected_cached_tokens, v_post_attempt_cached_tokens USING ERRCODE = '55000';
    END IF;

    IF v_post_attempt_media_units <> v_expected_media_units THEN
        RAISE EXCEPTION 'usage reconciliation parity mismatch: media_units expected %, got %',
            v_expected_media_units, v_post_attempt_media_units USING ERRCODE = '55000';
    END IF;

    IF v_post_attempt_cost <> v_expected_cost THEN
        RAISE EXCEPTION 'usage reconciliation parity mismatch: estimated_cost expected %, got %',
            v_expected_cost, v_post_attempt_cost USING ERRCODE = '55000';
    END IF;

    IF v_post_attempt_unpriced <> v_expected_unpriced THEN
        RAISE EXCEPTION 'usage reconciliation parity mismatch: unpriced_count expected %, got %',
            v_expected_unpriced, v_post_attempt_unpriced USING ERRCODE = '55000';
    END IF;

    IF v_post_attempt_incomplete <> v_expected_incomplete THEN
        RAISE EXCEPTION 'usage reconciliation parity mismatch: incomplete_count expected %, got %',
            v_expected_incomplete, v_post_attempt_incomplete USING ERRCODE = '55000';
    END IF;
END;
$reconciliation$;

-- Drop legacy triggers
DROP TRIGGER IF EXISTS usage_facts_attempt_mirror ON usage_facts;
DROP TRIGGER IF EXISTS usage_hourly_attempt_mirror ON usage_hourly;
DROP TRIGGER IF EXISTS usage_facts_attempt_legacy_archive ON usage_facts;
DROP TRIGGER IF EXISTS usage_facts_request_metadata_receipt_guard ON usage_facts;
DROP TRIGGER IF EXISTS usage_facts_preserve_request_metadata_receipt ON usage_facts;
DROP TRIGGER IF EXISTS usage_hourly_writer_guard ON usage_hourly;

-- Drop legacy trigger functions
DROP FUNCTION IF EXISTS mirror_legacy_usage_fact_to_attempt();
DROP FUNCTION IF EXISTS mirror_legacy_usage_hourly_to_attempt();
DROP FUNCTION IF EXISTS archive_attempt_usage_for_legacy_rollup();
DROP FUNCTION IF EXISTS enforce_request_metadata_fact_receipt();
DROP FUNCTION IF EXISTS preserve_request_metadata_fact_receipt();
DROP FUNCTION IF EXISTS enforce_usage_fact_receipt();
DROP FUNCTION IF EXISTS preserve_usage_fact_receipt();

-- Drop legacy tables
DROP TABLE usage_facts;
DROP TABLE usage_hourly;
