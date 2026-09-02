ALTER TABLE api_keys
    ADD COLUMN daily_cost_limit numeric(24, 12)
        CHECK (daily_cost_limit > 0),
    ADD COLUMN monthly_cost_limit numeric(24, 12)
        CHECK (monthly_cost_limit > 0);

ALTER TABLE attempt_usage_hourly
    ADD COLUMN unpriced_attempt_count bigint NOT NULL DEFAULT 0
        CHECK (unpriced_attempt_count >= 0);

CREATE TABLE api_key_cost_windows (
    api_key_id uuid NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    window_kind text NOT NULL CHECK (window_kind IN ('day', 'month')),
    window_id bigint NOT NULL CHECK (window_id >= 0),
    accrued numeric(28, 12) NOT NULL CHECK (accrued >= 0),
    unpriced_attempts bigint NOT NULL CHECK (unpriced_attempts >= 0),
    PRIMARY KEY (api_key_id, window_kind, window_id),
    CHECK (window_kind = 'month' OR unpriced_attempts = 0)
);

-- Older aggregates cannot distinguish repeated unpriced attempts against the
-- same target. Preserve their proven lower bound; all new rollups count the
-- underlying attempts exactly.
SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true);
UPDATE attempt_usage_hourly
SET unpriced_attempt_count = target_unpriced_count;

WITH bounds AS (
    SELECT date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS day_start,
           date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS month_start,
           (date_trunc('month', now() AT TIME ZONE 'UTC') + interval '1 month')
               AT TIME ZONE 'UTC' AS month_end
), usage AS (
    SELECT api_key_id, observed_at, COALESCE(estimated_cost, 0)::numeric AS cost,
           CASE WHEN charge_status <> 'not_billable' AND unpriced
                THEN 1 ELSE 0 END::bigint AS unpriced_attempts
    FROM attempt_usage_facts, bounds
    WHERE observed_at >= bounds.month_start AND observed_at < bounds.month_end
    UNION ALL
    SELECT api_key_id, bucket, COALESCE(estimated_cost, 0)::numeric,
           unpriced_attempt_count
    FROM attempt_usage_hourly, bounds
    WHERE api_key_id IS NOT NULL
      AND bucket >= bounds.month_start AND bucket < bounds.month_end
), totals AS (
    SELECT usage.api_key_id,
           COALESCE(sum(usage.cost) FILTER (
               WHERE usage.observed_at >= bounds.day_start
                 AND usage.observed_at < bounds.day_start + interval '24 hours'
           ), 0)
               AS daily_accrued,
           COALESCE(sum(usage.cost), 0) AS monthly_accrued,
           COALESCE(sum(usage.unpriced_attempts), 0)::bigint AS monthly_unpriced_attempts,
           bounds.day_start, bounds.month_start
    FROM usage CROSS JOIN bounds
    GROUP BY usage.api_key_id, bounds.day_start, bounds.month_start
)
INSERT INTO api_key_cost_windows (
    api_key_id, window_kind, window_id, accrued, unpriced_attempts
)
SELECT api_key_id, 'day', floor(extract(epoch FROM day_start) / 86400)::bigint,
       daily_accrued, 0
FROM totals
UNION ALL
SELECT api_key_id, 'month', extract(year FROM month_start)::bigint * 12
       + extract(month FROM month_start)::bigint - 1, monthly_accrued,
       monthly_unpriced_attempts
FROM totals;

-- The existing time-first indexes scanned 49,500 irrelevant rows per source
-- in the representative key-window plan; these narrowed each scan to 500.
CREATE INDEX attempt_usage_facts_api_key_observed_at_idx
    ON attempt_usage_facts(api_key_id, observed_at DESC);
CREATE INDEX attempt_usage_hourly_api_key_bucket_idx
    ON attempt_usage_hourly(api_key_id, bucket DESC)
    WHERE api_key_id IS NOT NULL;

-- A pre-0049 maintenance worker cannot populate the exact attempt count. Fence
-- its whole rollup transaction so raw facts remain for a current worker.
CREATE OR REPLACE FUNCTION enforce_usage_rollup_writer() RETURNS trigger AS $$
BEGIN
    IF current_setting('olp.usage_rollup_writer', true) IS DISTINCT FROM 'additive-v3' THEN
        RAISE EXCEPTION 'usage rollup requires the additive writer fence'
            USING ERRCODE = '55000';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION mirror_legacy_usage_hourly_to_attempt() RETURNS trigger AS $$
DECLARE
    request_delta bigint;
    input_delta numeric(30, 0);
    output_delta numeric(30, 0);
    cached_delta numeric(30, 0);
    media_delta numeric(30, 6);
    cost_delta numeric(30, 12);
    unpriced_delta bigint;
    incomplete_delta bigint;
BEGIN
    IF current_setting('olp.attempt_usage_hourly_mirror', true) = 'off' THEN
        RETURN NEW;
    END IF;

    request_delta := NEW.request_count - CASE WHEN TG_OP = 'UPDATE' THEN OLD.request_count ELSE 0 END;
    input_delta := NEW.input_tokens - CASE WHEN TG_OP = 'UPDATE' THEN OLD.input_tokens ELSE 0 END;
    output_delta := NEW.output_tokens - CASE WHEN TG_OP = 'UPDATE' THEN OLD.output_tokens ELSE 0 END;
    cached_delta := NEW.cached_input_tokens - CASE WHEN TG_OP = 'UPDATE' THEN OLD.cached_input_tokens ELSE 0 END;
    media_delta := NEW.media_units - CASE WHEN TG_OP = 'UPDATE' THEN OLD.media_units ELSE 0 END;
    unpriced_delta := NEW.unpriced_count - CASE WHEN TG_OP = 'UPDATE' THEN OLD.unpriced_count ELSE 0 END;
    incomplete_delta := NEW.incomplete_count - CASE WHEN TG_OP = 'UPDATE' THEN OLD.incomplete_count ELSE 0 END;
    cost_delta := CASE
        WHEN NEW.estimated_cost IS NULL THEN NULL
        WHEN TG_OP = 'UPDATE' AND OLD.estimated_cost IS NOT NULL
            THEN NEW.estimated_cost - OLD.estimated_cost
        ELSE NEW.estimated_cost
    END;

    INSERT INTO attempt_usage_hourly (
        bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
        request_count, provider_request_count, model_request_count, target_request_count,
        input_tokens, output_tokens, cached_input_tokens, media_units, estimated_cost,
        request_unpriced_count, provider_unpriced_count, model_unpriced_count,
        target_unpriced_count, unpriced_attempt_count, request_incomplete_count,
        provider_incomplete_count, model_incomplete_count, target_incomplete_count, currency
    ) VALUES (
        NEW.bucket, NEW.route_slug, NEW.provider_id, NEW.upstream_model, NEW.operation,
        NEW.surface, NEW.api_key_id, request_delta, request_delta, request_delta,
        request_delta, input_delta, output_delta, cached_delta, media_delta, cost_delta,
        unpriced_delta, unpriced_delta, unpriced_delta, unpriced_delta, unpriced_delta,
        incomplete_delta, incomplete_delta, incomplete_delta, incomplete_delta, NEW.currency
    )
    ON CONFLICT (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id)
    DO UPDATE SET
        request_count = attempt_usage_hourly.request_count + EXCLUDED.request_count,
        provider_request_count = attempt_usage_hourly.provider_request_count + EXCLUDED.provider_request_count,
        model_request_count = attempt_usage_hourly.model_request_count + EXCLUDED.model_request_count,
        target_request_count = attempt_usage_hourly.target_request_count + EXCLUDED.target_request_count,
        input_tokens = attempt_usage_hourly.input_tokens + EXCLUDED.input_tokens,
        output_tokens = attempt_usage_hourly.output_tokens + EXCLUDED.output_tokens,
        cached_input_tokens = attempt_usage_hourly.cached_input_tokens + EXCLUDED.cached_input_tokens,
        media_units = attempt_usage_hourly.media_units + EXCLUDED.media_units,
        estimated_cost = CASE
            WHEN attempt_usage_hourly.estimated_cost IS NULL
                 AND EXCLUDED.estimated_cost IS NULL THEN NULL
            ELSE COALESCE(attempt_usage_hourly.estimated_cost, 0)
                 + COALESCE(EXCLUDED.estimated_cost, 0)
        END,
        request_unpriced_count = attempt_usage_hourly.request_unpriced_count + EXCLUDED.request_unpriced_count,
        provider_unpriced_count = attempt_usage_hourly.provider_unpriced_count + EXCLUDED.provider_unpriced_count,
        model_unpriced_count = attempt_usage_hourly.model_unpriced_count + EXCLUDED.model_unpriced_count,
        target_unpriced_count = attempt_usage_hourly.target_unpriced_count + EXCLUDED.target_unpriced_count,
        unpriced_attempt_count = attempt_usage_hourly.unpriced_attempt_count
                                 + EXCLUDED.unpriced_attempt_count,
        request_incomplete_count = attempt_usage_hourly.request_incomplete_count + EXCLUDED.request_incomplete_count,
        provider_incomplete_count = attempt_usage_hourly.provider_incomplete_count + EXCLUDED.provider_incomplete_count,
        model_incomplete_count = attempt_usage_hourly.model_incomplete_count + EXCLUDED.model_incomplete_count,
        target_incomplete_count = attempt_usage_hourly.target_incomplete_count + EXCLUDED.target_incomplete_count,
        currency = COALESCE(attempt_usage_hourly.currency, EXCLUDED.currency);

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION archive_attempt_usage_for_legacy_rollup() RETURNS trigger AS $$
BEGIN
    IF current_setting('olp.usage_rollup_writer', true) IS DISTINCT FROM 'additive-v3' OR
       current_setting('olp.attempt_usage_legacy_archive', true) = 'off' THEN
        RETURN OLD;
    END IF;

    PERFORM set_config('olp.attempt_usage_hourly_mirror', 'off', true);
    WITH expired AS (
        DELETE FROM attempt_usage_facts
         WHERE request_id = OLD.request_id
           AND request_started_at = OLD.request_started_at
        RETURNING *
    )
    INSERT INTO attempt_usage_hourly (
        bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
        request_count, provider_request_count, model_request_count, target_request_count,
        input_tokens, output_tokens, cached_input_tokens, media_units, estimated_cost,
        request_unpriced_count, provider_unpriced_count, model_unpriced_count,
        target_unpriced_count, unpriced_attempt_count, request_incomplete_count,
        provider_incomplete_count, model_incomplete_count, target_incomplete_count, currency
    )
    SELECT date_trunc('hour', observed_at), route_slug, provider_id, upstream_model,
           operation, surface, api_key_id,
           count(*) FILTER (WHERE request_counted),
           count(*) FILTER (WHERE provider_request_counted),
           count(*) FILTER (WHERE model_request_counted),
           count(*) FILTER (WHERE target_request_counted),
           COALESCE(sum(input_tokens), 0), COALESCE(sum(output_tokens), 0),
           COALESCE(sum(cached_input_tokens), 0), COALESCE(sum(media_units), 0),
           sum(estimated_cost),
           count(*) FILTER (WHERE request_unpriced_counted),
           count(*) FILTER (WHERE provider_unpriced_counted),
           count(*) FILTER (WHERE model_unpriced_counted),
           count(*) FILTER (WHERE target_unpriced_counted),
           count(*) FILTER (WHERE charge_status <> 'not_billable' AND unpriced),
           count(*) FILTER (WHERE request_incomplete_counted),
           count(*) FILTER (WHERE provider_incomplete_counted),
           count(*) FILTER (WHERE model_incomplete_counted),
           count(*) FILTER (WHERE target_incomplete_counted), max(currency)
      FROM expired
     GROUP BY date_trunc('hour', observed_at), route_slug, provider_id, upstream_model,
              operation, surface, api_key_id
    ON CONFLICT ON CONSTRAINT attempt_usage_hourly_dimensions_key DO UPDATE SET
        request_count = attempt_usage_hourly.request_count + EXCLUDED.request_count,
        provider_request_count = attempt_usage_hourly.provider_request_count
                                 + EXCLUDED.provider_request_count,
        model_request_count = attempt_usage_hourly.model_request_count
                              + EXCLUDED.model_request_count,
        target_request_count = attempt_usage_hourly.target_request_count
                               + EXCLUDED.target_request_count,
        input_tokens = attempt_usage_hourly.input_tokens + EXCLUDED.input_tokens,
        output_tokens = attempt_usage_hourly.output_tokens + EXCLUDED.output_tokens,
        cached_input_tokens = attempt_usage_hourly.cached_input_tokens
                              + EXCLUDED.cached_input_tokens,
        media_units = attempt_usage_hourly.media_units + EXCLUDED.media_units,
        estimated_cost = CASE
            WHEN attempt_usage_hourly.estimated_cost IS NULL
                 AND EXCLUDED.estimated_cost IS NULL THEN NULL
            ELSE COALESCE(attempt_usage_hourly.estimated_cost, 0)
                 + COALESCE(EXCLUDED.estimated_cost, 0)
        END,
        request_unpriced_count = attempt_usage_hourly.request_unpriced_count
                                 + EXCLUDED.request_unpriced_count,
        provider_unpriced_count = attempt_usage_hourly.provider_unpriced_count
                                  + EXCLUDED.provider_unpriced_count,
        model_unpriced_count = attempt_usage_hourly.model_unpriced_count
                               + EXCLUDED.model_unpriced_count,
        target_unpriced_count = attempt_usage_hourly.target_unpriced_count
                                + EXCLUDED.target_unpriced_count,
        unpriced_attempt_count = attempt_usage_hourly.unpriced_attempt_count
                                 + EXCLUDED.unpriced_attempt_count,
        request_incomplete_count = attempt_usage_hourly.request_incomplete_count
                                   + EXCLUDED.request_incomplete_count,
        provider_incomplete_count = attempt_usage_hourly.provider_incomplete_count
                                    + EXCLUDED.provider_incomplete_count,
        model_incomplete_count = attempt_usage_hourly.model_incomplete_count
                                 + EXCLUDED.model_incomplete_count,
        target_incomplete_count = attempt_usage_hourly.target_incomplete_count
                                  + EXCLUDED.target_incomplete_count,
        currency = COALESCE(attempt_usage_hourly.currency, EXCLUDED.currency);

    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE worker_task_health
    DROP CONSTRAINT worker_task_health_task_check,
    ADD CONSTRAINT worker_task_health_task_check CHECK (task IN (
        'runtime_outbox',
        'request_metadata_consumer',
        'maintenance',
        'request_metadata_gateway_epoch_detection',
        'cost_reconciliation'
    ));
