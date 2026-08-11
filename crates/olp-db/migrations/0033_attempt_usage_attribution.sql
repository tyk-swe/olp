-- A logical request can execute more than one provider attempt. Preserve
-- usage, billing uncertainty, and the immutable pricing decision on the
-- attempt that produced them instead of assigning a request aggregate to the
-- final target.
CREATE TYPE attempt_charge_status AS ENUM
    ('not_billable', 'billable', 'billing_uncertain');

CREATE TABLE attempt_usage_facts (
    attempt_id uuid PRIMARY KEY,
    event_id uuid NOT NULL,
    request_id uuid NOT NULL,
    request_started_at timestamptz NOT NULL,
    attempt_ordinal smallint NOT NULL CHECK (attempt_ordinal > 0),
    api_key_id uuid NOT NULL REFERENCES api_keys(id),
    provider_id uuid NOT NULL REFERENCES providers(id),
    route_slug text NOT NULL,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    attempt_started_at timestamptz NOT NULL,
    attempt_completed_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    charge_status attempt_charge_status NOT NULL,
    usage_observed boolean NOT NULL,
    usage_complete boolean NOT NULL,
    input_tokens bigint,
    output_tokens bigint,
    cached_input_tokens bigint,
    media_units numeric(24, 6),
    estimated_cost numeric(24, 12),
    unpriced boolean NOT NULL,
    pricing_revision_id uuid REFERENCES pricing_revisions(id),
    currency char(3),
    request_counted boolean NOT NULL,
    provider_request_counted boolean NOT NULL,
    model_request_counted boolean NOT NULL,
    target_request_counted boolean NOT NULL,
    request_unpriced_counted boolean NOT NULL,
    provider_unpriced_counted boolean NOT NULL,
    model_unpriced_counted boolean NOT NULL,
    target_unpriced_counted boolean NOT NULL,
    request_incomplete_counted boolean NOT NULL,
    provider_incomplete_counted boolean NOT NULL,
    model_incomplete_counted boolean NOT NULL,
    target_incomplete_counted boolean NOT NULL,
    UNIQUE (request_id, attempt_ordinal),
    FOREIGN KEY (request_id, request_started_at)
        REFERENCES usage_request_anchors(request_id, request_started_at)
        ON DELETE CASCADE,
    CHECK (surface IN ('openai', 'anthropic', 'gemini', 'unknown')),
    CHECK (attempt_completed_at >= attempt_started_at),
    CHECK (input_tokens IS NULL OR input_tokens >= 0),
    CHECK (output_tokens IS NULL OR output_tokens >= 0),
    CHECK (cached_input_tokens IS NULL OR cached_input_tokens >= 0),
    CHECK (media_units IS NULL OR media_units >= 0),
    CHECK (estimated_cost IS NULL OR estimated_cost >= 0),
    CHECK (currency IS NULL OR
           (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$')),
    CHECK (charge_status <> 'not_billable' OR
           (NOT usage_observed AND usage_complete AND NOT unpriced AND
            estimated_cost IS NULL AND pricing_revision_id IS NULL)),
    CHECK (charge_status <> 'billing_uncertain' OR NOT usage_complete),
    CHECK (estimated_cost IS NULL OR
           (charge_status = 'billable' AND usage_complete AND NOT unpriced))
);

CREATE INDEX attempt_usage_facts_observed_at_idx
    ON attempt_usage_facts(observed_at DESC);
CREATE INDEX attempt_usage_facts_request_idx
    ON attempt_usage_facts(request_id, request_started_at, attempt_ordinal);
CREATE INDEX attempt_usage_facts_provider_idx
    ON attempt_usage_facts(provider_id, observed_at DESC);
CREATE INDEX attempt_usage_facts_model_idx
    ON attempt_usage_facts(upstream_model, observed_at DESC);

-- Count markers make request counts additive without assigning usage to an
-- arbitrary attempt. The request marker appears once per logical request;
-- provider/model/target markers appear once per distinct attribution scope.
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
)
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
  FROM marked;

CREATE TABLE attempt_usage_hourly (
    bucket timestamptz NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL REFERENCES providers(id),
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    api_key_id uuid REFERENCES api_keys(id),
    request_count bigint NOT NULL CHECK (request_count >= 0),
    provider_request_count bigint NOT NULL CHECK (provider_request_count >= 0),
    model_request_count bigint NOT NULL CHECK (model_request_count >= 0),
    target_request_count bigint NOT NULL CHECK (target_request_count >= 0),
    input_tokens numeric(30, 0) NOT NULL CHECK (input_tokens >= 0),
    output_tokens numeric(30, 0) NOT NULL CHECK (output_tokens >= 0),
    cached_input_tokens numeric(30, 0) NOT NULL CHECK (cached_input_tokens >= 0),
    media_units numeric(30, 6) NOT NULL CHECK (media_units >= 0),
    estimated_cost numeric(30, 12) CHECK (estimated_cost >= 0),
    request_unpriced_count bigint NOT NULL CHECK (request_unpriced_count >= 0),
    provider_unpriced_count bigint NOT NULL CHECK (provider_unpriced_count >= 0),
    model_unpriced_count bigint NOT NULL CHECK (model_unpriced_count >= 0),
    target_unpriced_count bigint NOT NULL CHECK (target_unpriced_count >= 0),
    request_incomplete_count bigint NOT NULL CHECK (request_incomplete_count >= 0),
    provider_incomplete_count bigint NOT NULL CHECK (provider_incomplete_count >= 0),
    model_incomplete_count bigint NOT NULL CHECK (model_incomplete_count >= 0),
    target_incomplete_count bigint NOT NULL CHECK (target_incomplete_count >= 0),
    currency char(3),
    CHECK (surface IN ('openai', 'anthropic', 'gemini', 'unknown')),
    CHECK (currency IS NULL OR
           (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$')),
    CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT
        (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id)
);

-- Cold aggregates predate attempt attribution. Seed the new retained source
-- one-for-one without inventing additional providers or attempts.
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
  FROM usage_hourly;

CREATE TRIGGER attempt_usage_hourly_writer_guard
    BEFORE INSERT OR UPDATE ON attempt_usage_hourly
    FOR EACH STATEMENT EXECUTE FUNCTION enforce_usage_rollup_writer();

-- An older maintenance worker can still write the compatibility hourly table
-- while a rolling upgrade is in progress. Mirror only its additive delta into
-- the attempt-authoritative aggregate. Current workers disable this trigger
-- around their compatibility write because they have already rolled the
-- authoritative attempt facts directly.
CREATE FUNCTION mirror_legacy_usage_hourly_to_attempt() RETURNS trigger AS $$
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
        target_unpriced_count, request_incomplete_count, provider_incomplete_count,
        model_incomplete_count, target_incomplete_count, currency
    ) VALUES (
        NEW.bucket, NEW.route_slug, NEW.provider_id, NEW.upstream_model, NEW.operation,
        NEW.surface, NEW.api_key_id, request_delta, request_delta, request_delta,
        request_delta, input_delta, output_delta, cached_delta, media_delta, cost_delta,
        unpriced_delta, unpriced_delta, unpriced_delta, unpriced_delta,
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
        request_incomplete_count = attempt_usage_hourly.request_incomplete_count + EXCLUDED.request_incomplete_count,
        provider_incomplete_count = attempt_usage_hourly.provider_incomplete_count + EXCLUDED.provider_incomplete_count,
        model_incomplete_count = attempt_usage_hourly.model_incomplete_count + EXCLUDED.model_incomplete_count,
        target_incomplete_count = attempt_usage_hourly.target_incomplete_count + EXCLUDED.target_incomplete_count,
        currency = COALESCE(attempt_usage_hourly.currency, EXCLUDED.currency);

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER usage_hourly_attempt_mirror
    AFTER INSERT OR UPDATE ON usage_hourly
    FOR EACH ROW EXECUTE FUNCTION mirror_legacy_usage_hourly_to_attempt();

-- An N-1 maintenance worker deletes compatibility facts before it updates the
-- legacy hourly table. Archive their authoritative attempt rows here, then
-- disable the row-level hourly mirror for the rest of that transaction. This
-- preserves multi-attempt attribution and prevents a later current worker
-- from rolling the same attempt rows a second time.
CREATE FUNCTION archive_attempt_usage_for_legacy_rollup() RETURNS trigger AS $$
BEGIN
    IF current_setting('olp.usage_rollup_writer', true) IS DISTINCT FROM 'additive-v2' OR
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
        target_unpriced_count, request_incomplete_count, provider_incomplete_count,
        model_incomplete_count, target_incomplete_count, currency
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

CREATE TRIGGER usage_facts_attempt_legacy_archive
    AFTER DELETE ON usage_facts
    FOR EACH ROW EXECUTE FUNCTION archive_attempt_usage_for_legacy_rollup();

-- During a rolling upgrade an older writer can still insert only the legacy
-- request aggregate. Mirror it into the authoritative table when no current
-- writer has already supplied attempt facts. The old payload cannot recover
-- earlier attempt usage, so this fallback remains explicitly incomplete when
-- no reliable usage was retained.
CREATE FUNCTION mirror_legacy_usage_fact_to_attempt() RETURNS trigger AS $$
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
        attempt_started_at, attempt_completed_at, observed_at, charge_status,
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
           NEW.surface, started_at, GREATEST(completed_at, started_at), NEW.observed_at,
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

CREATE TRIGGER usage_facts_attempt_mirror
    AFTER INSERT ON usage_facts
    FOR EACH ROW EXECUTE FUNCTION mirror_legacy_usage_fact_to_attempt();
