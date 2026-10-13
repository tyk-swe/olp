-- Pricing dimensions: modality, per-request, tool, tier, window and batch
-- price components, and the attempt counters they price.
ALTER TABLE olp.prices
    ADD COLUMN audio_input_per_million numeric(24,12),
    ADD COLUMN cached_audio_input_per_million numeric(24,12),
    ADD COLUMN audio_output_per_million numeric(24,12),
    ADD COLUMN image_input_per_million numeric(24,12),
    ADD COLUMN cached_image_input_per_million numeric(24,12),
    ADD COLUMN image_output_per_million numeric(24,12),
    ADD COLUMN per_request numeric(24,12),
    ADD COLUMN batch_multiplier numeric(24,12),
    ADD COLUMN tool_calls jsonb
        CHECK (tool_calls IS NULL OR (jsonb_typeof(tool_calls) = 'object'
               AND octet_length(tool_calls::text) <= 4096)),
    ADD COLUMN tiers jsonb
        CHECK (tiers IS NULL OR (jsonb_typeof(tiers) = 'array'
               AND octet_length(tiers::text) <= 16384)),
    ADD COLUMN windows jsonb
        CHECK (windows IS NULL OR (jsonb_typeof(windows) = 'object'
               AND octet_length(windows::text) <= 8192));

ALTER TABLE olp.attempt_usage_facts
    ADD COLUMN audio_input_tokens bigint
        CHECK (audio_input_tokens IS NULL OR audio_input_tokens >= 0),
    ADD COLUMN cached_audio_input_tokens bigint
        CHECK (cached_audio_input_tokens IS NULL OR cached_audio_input_tokens >= 0),
    ADD COLUMN audio_output_tokens bigint
        CHECK (audio_output_tokens IS NULL OR audio_output_tokens >= 0),
    ADD COLUMN image_input_tokens bigint
        CHECK (image_input_tokens IS NULL OR image_input_tokens >= 0),
    ADD COLUMN cached_image_input_tokens bigint
        CHECK (cached_image_input_tokens IS NULL OR cached_image_input_tokens >= 0),
    ADD COLUMN image_output_tokens bigint
        CHECK (image_output_tokens IS NULL OR image_output_tokens >= 0),
    ADD COLUMN tool_calls jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(tool_calls) = 'object'
               AND octet_length(tool_calls::text) <= 1024),
    ADD COLUMN batch boolean NOT NULL DEFAULT false,
    ADD COLUMN pricing_components text[],
    ADD CONSTRAINT attempt_usage_modality_subset CHECK (
        (input_tokens IS NOT NULL OR (audio_input_tokens IS NULL AND
            cached_audio_input_tokens IS NULL AND image_input_tokens IS NULL AND
            cached_image_input_tokens IS NULL))
        AND (output_tokens IS NOT NULL OR
            (audio_output_tokens IS NULL AND image_output_tokens IS NULL))
        AND COALESCE(cached_audio_input_tokens, 0) <= COALESCE(audio_input_tokens, 0)
        AND COALESCE(cached_image_input_tokens, 0) <= COALESCE(image_input_tokens, 0)
        AND COALESCE(cached_audio_input_tokens, 0) + COALESCE(cached_image_input_tokens, 0)
            <= COALESCE(cached_input_tokens, 0)
        AND (input_tokens IS NULL OR
            COALESCE(cached_input_tokens, 0) + COALESCE(cache_write_input_tokens, 0)
                + (COALESCE(audio_input_tokens, 0) - COALESCE(cached_audio_input_tokens, 0))
                + (COALESCE(image_input_tokens, 0) - COALESCE(cached_image_input_tokens, 0))
            <= input_tokens)
        AND (output_tokens IS NULL OR
            COALESCE(audio_output_tokens, 0) + COALESCE(image_output_tokens, 0) <= output_tokens));

ALTER TABLE olp.attempt_usage_hourly
    ADD COLUMN audio_input_tokens numeric(30,0) NOT NULL DEFAULT 0
        CHECK (audio_input_tokens >= 0),
    ADD COLUMN cached_audio_input_tokens numeric(30,0) NOT NULL DEFAULT 0
        CHECK (cached_audio_input_tokens >= 0),
    ADD COLUMN audio_output_tokens numeric(30,0) NOT NULL DEFAULT 0
        CHECK (audio_output_tokens >= 0),
    ADD COLUMN image_input_tokens numeric(30,0) NOT NULL DEFAULT 0
        CHECK (image_input_tokens >= 0),
    ADD COLUMN cached_image_input_tokens numeric(30,0) NOT NULL DEFAULT 0
        CHECK (cached_image_input_tokens >= 0),
    ADD COLUMN image_output_tokens numeric(30,0) NOT NULL DEFAULT 0
        CHECK (image_output_tokens >= 0);

-- olp.price_first_rate resolves one share's rate through its fallback chain:
-- the first field the rate set carries, or nothing.
CREATE FUNCTION olp.price_first_rate(rates jsonb, VARIADIC names text[])
RETURNS TABLE(rate numeric, field text) LANGUAGE sql IMMUTABLE AS $$
    SELECT (rates->>name)::numeric(24,12), name FROM unnest(names) AS name
    WHERE rates->>name IS NOT NULL LIMIT 1
$$;

-- olp.price_usage prices one attempt's usage document against one price row.
-- `price` is the olp.prices row as jsonb, `usage` the attempt's usage document,
-- and `discount` a multiplier applied after the list cost (defaults to 1). The
-- discount is part of the evidence when a multiplier other than one applied.
-- The attempt is complete only when every dimension it used resolves
-- to a rate; anything else returns complete = false with NULL amounts.
CREATE FUNCTION olp.price_usage(price jsonb, usage jsonb, started_at timestamptz,
                                successful boolean, discount numeric DEFAULT 1)
RETURNS TABLE(complete boolean, list_cost numeric, cost numeric, components text[])
LANGUAGE plpgsql STABLE AS $$
DECLARE
    rates jsonb := price;
    tier jsonb;
    v_input bigint := (usage->>'input_tokens')::bigint;
    v_output bigint := (usage->>'output_tokens')::bigint;
    v_cached bigint := COALESCE((usage->>'cached_input_tokens')::bigint, 0);
    v_write bigint := COALESCE((usage->>'cache_write_input_tokens')::bigint, 0);
    v_w5m bigint := COALESCE((usage->>'cache_write_5m_input_tokens')::bigint, 0);
    v_w1h bigint := COALESCE((usage->>'cache_write_1h_input_tokens')::bigint, 0);
    v_audio bigint := COALESCE((usage->>'audio_input_tokens')::bigint, 0);
    v_caudio bigint := COALESCE((usage->>'cached_audio_input_tokens')::bigint, 0);
    v_image bigint := COALESCE((usage->>'image_input_tokens')::bigint, 0);
    v_cimage bigint := COALESCE((usage->>'cached_image_input_tokens')::bigint, 0);
    v_aout bigint := COALESCE((usage->>'audio_output_tokens')::bigint, 0);
    v_iout bigint := COALESCE((usage->>'image_output_tokens')::bigint, 0);
    v_media numeric := (usage->>'media_units')::numeric;
    v_batch boolean := COALESCE((usage->>'batch')::boolean, false);
    v_ok boolean := price IS NOT NULL;
    v_in_charge numeric := 0;
    v_out_charge numeric := 0;
    v_charge numeric := 0;
    v_multiplier numeric := 1;
    v_comp text[] := '{}';
    share record;
    tool record;
    share_rate numeric;
    share_field text;
    fee numeric;
    local_ts timestamp;
    dow int;
    tod int;
    window_multiplier numeric;
BEGIN
    IF price IS NULL THEN
        RETURN QUERY SELECT false, NULL::numeric, NULL::numeric, NULL::text[];
        RETURN;
    END IF;
    -- A tier replaces the whole per-million rate set for the attempt whose
    -- input exceeds its threshold; the highest exceeded threshold wins.
    IF v_input IS NOT NULL AND jsonb_typeof(price->'tiers') = 'array' THEN
        SELECT t INTO tier FROM jsonb_array_elements(price->'tiers') AS t
        WHERE (t->>'above_input_tokens')::bigint < v_input
        ORDER BY (t->>'above_input_tokens')::bigint DESC LIMIT 1;
        IF FOUND THEN
            rates := tier;
            v_comp := v_comp || 'tiers'::text;
        END IF;
    END IF;

    -- Input side: every share bills at the first rate its chain carries, and
    -- a nonzero share with no rate leaves the attempt unpriced.
    IF v_input IS NULL THEN
        IF usage->>'cached_input_tokens' IS NOT NULL OR usage->>'cache_write_input_tokens' IS NOT NULL
            OR usage->>'cache_write_5m_input_tokens' IS NOT NULL OR usage->>'cache_write_1h_input_tokens' IS NOT NULL
            OR usage->>'audio_input_tokens' IS NOT NULL OR usage->>'cached_audio_input_tokens' IS NOT NULL
            OR usage->>'image_input_tokens' IS NOT NULL OR usage->>'cached_image_input_tokens' IS NOT NULL THEN
            v_ok := false;
        END IF;
    ELSE
        IF rates->>'input_per_million' IS NULL AND rates->>'audio_input_per_million' IS NULL
            AND rates->>'image_input_per_million' IS NULL THEN
            v_ok := false;
        END IF;
        FOR share IN
            SELECT * FROM (VALUES
                (v_input - v_cached - v_write - (v_audio - v_caudio) - (v_image - v_cimage),
                    ARRAY['input_per_million']),
                (v_cached - v_caudio - v_cimage,
                    ARRAY['cached_input_per_million','input_per_million']),
                (v_caudio,
                    ARRAY['cached_audio_input_per_million','cached_input_per_million','input_per_million']),
                (v_cimage,
                    ARRAY['cached_image_input_per_million','cached_input_per_million','input_per_million']),
                (v_audio - v_caudio,
                    ARRAY['audio_input_per_million','input_per_million']),
                (v_image - v_cimage,
                    ARRAY['image_input_per_million','input_per_million']),
                (v_write - v_w5m - v_w1h,
                    ARRAY['cache_write_input_per_million','input_per_million']),
                (v_w5m,
                    ARRAY['cache_write_5m_input_per_million','cache_write_input_per_million','input_per_million']),
                (v_w1h,
                    ARRAY['cache_write_1h_input_per_million','cache_write_input_per_million','input_per_million'])
            ) AS s(tokens, chain)
        LOOP
            IF share.tokens < 0 THEN
                v_ok := false;
                CONTINUE;
            END IF;
            share_rate := NULL;
            share_field := NULL;
            SELECT r.rate, r.field INTO share_rate, share_field
                FROM olp.price_first_rate(rates, VARIADIC share.chain) r;
            IF share_rate IS NULL THEN
                IF share.tokens > 0 THEN
                    v_ok := false;
                END IF;
            ELSE
                v_in_charge := v_in_charge + share.tokens * share_rate;
                IF share.tokens > 0 AND NOT share_field = ANY(v_comp) THEN
                    v_comp := v_comp || share_field;
                END IF;
            END IF;
        END LOOP;
    END IF;

    -- Output side, with the same share and fallback rules.
    IF v_output IS NULL THEN
        IF usage->>'audio_output_tokens' IS NOT NULL OR usage->>'image_output_tokens' IS NOT NULL THEN
            v_ok := false;
        END IF;
    ELSE
        IF rates->>'output_per_million' IS NULL AND rates->>'audio_output_per_million' IS NULL
            AND rates->>'image_output_per_million' IS NULL THEN
            v_ok := false;
        END IF;
        FOR share IN
            SELECT * FROM (VALUES
                (v_output - v_aout - v_iout, ARRAY['output_per_million']),
                (v_aout, ARRAY['audio_output_per_million','output_per_million']),
                (v_iout, ARRAY['image_output_per_million','output_per_million'])
            ) AS s(tokens, chain)
        LOOP
            IF share.tokens < 0 THEN
                v_ok := false;
                CONTINUE;
            END IF;
            share_rate := NULL;
            share_field := NULL;
            SELECT r.rate, r.field INTO share_rate, share_field
                FROM olp.price_first_rate(rates, VARIADIC share.chain) r;
            IF share_rate IS NULL THEN
                IF share.tokens > 0 THEN
                    v_ok := false;
                END IF;
            ELSE
                v_out_charge := v_out_charge + share.tokens * share_rate;
                IF share.tokens > 0 AND NOT share_field = ANY(v_comp) THEN
                    v_comp := v_comp || share_field;
                END IF;
            END IF;
        END LOOP;
    END IF;

    -- Each side is divided by a million once, the way the rates read.
    v_charge := COALESCE(v_in_charge / 1000000, 0) + COALESCE(v_out_charge / 1000000, 0);

    IF v_media IS NOT NULL THEN
        IF price->>'unit_price' IS NULL THEN
            v_ok := false;
        ELSE
            v_charge := v_charge + v_media * (price->>'unit_price')::numeric;
            IF v_media > 0 THEN
                v_comp := v_comp || 'unit_price'::text;
            END IF;
        END IF;
    END IF;

    IF successful AND price->>'per_request' IS NOT NULL THEN
        v_charge := v_charge + (price->>'per_request')::numeric;
        v_comp := v_comp || 'per_request'::text;
    END IF;

    IF jsonb_typeof(usage->'tool_calls') = 'object' THEN
        FOR tool IN SELECT key, value::bigint AS calls
            FROM jsonb_each_text(usage->'tool_calls') WHERE value::bigint > 0
        LOOP
            fee := (price->'tool_calls'->>tool.key)::numeric;
            IF fee IS NULL THEN
                v_ok := false;
            ELSE
                v_charge := v_charge + tool.calls * fee;
                v_comp := v_comp || ('tool_calls.' || tool.key);
            END IF;
        END LOOP;
    END IF;

    -- The window multiplier that holds at the attempt's start in the zone the
    -- price names; periods never overlap on a shared day, so at most one does.
    IF jsonb_typeof(price->'windows') = 'object' THEN
        local_ts := started_at AT TIME ZONE (price->'windows'->>'time_zone');
        dow := EXTRACT(ISODOW FROM local_ts)::int;
        tod := EXTRACT(HOUR FROM local_ts)::int * 60 + EXTRACT(MINUTE FROM local_ts)::int;
        window_multiplier := NULL;
        SELECT (p->>'multiplier')::numeric INTO window_multiplier
        FROM jsonb_array_elements(price->'windows'->'periods') AS p
        WHERE EXISTS (SELECT 1 FROM jsonb_array_elements_text(p->'days') d
                      WHERE d::int = dow)
          AND tod >= split_part(p->>'start', ':', 1)::int * 60 + split_part(p->>'start', ':', 2)::int
          AND tod < split_part(p->>'end', ':', 1)::int * 60 + split_part(p->>'end', ':', 2)::int
        LIMIT 1;
        IF FOUND THEN
            v_multiplier := v_multiplier * window_multiplier;
            v_comp := v_comp || 'windows'::text;
        END IF;
    END IF;

    IF v_batch THEN
        IF price->>'batch_multiplier' IS NULL THEN
            v_ok := false;
        ELSE
            v_multiplier := v_multiplier * (price->>'batch_multiplier')::numeric;
            v_comp := v_comp || 'batch_multiplier'::text;
        END IF;
    END IF;

    IF COALESCE(discount, 1) <> 1 THEN
        v_comp := v_comp || 'discount'::text;
    END IF;
    v_ok := v_ok AND COALESCE((usage->>'complete')::boolean, false);
    IF NOT v_ok THEN
        RETURN QUERY SELECT false, NULL::numeric, NULL::numeric, NULL::text[];
        RETURN;
    END IF;
    RETURN QUERY SELECT true,
        round(v_charge * v_multiplier, 12),
        round(v_charge * v_multiplier * COALESCE(discount, 1), 12),
        (SELECT array_agg(c ORDER BY c) FROM (SELECT DISTINCT unnest(v_comp) AS c) sorted);
END $$;

CREATE OR REPLACE FUNCTION olp.export_accounted_request() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_request olp.requests%ROWTYPE; v_project uuid; v_outcome text; v_attempt record; v_rollup record;
BEGIN
 IF NEW.status<>'fact_persisted' OR OLD.status='fact_persisted' THEN RETURN NEW; END IF;
 IF NOT EXISTS(SELECT 1 FROM olp.export_sinks WHERE enabled AND streams && ARRAY['requests','attempts','usage_rollups','guardrail_decisions']::text[]) THEN RETURN NEW; END IF;
 SELECT r.* INTO v_request FROM olp.requests r WHERE r.id=NEW.request_id;
 IF NOT FOUND THEN RETURN NEW; END IF;
 SELECT COALESCE(k.project_id,rt.project_id) INTO v_project FROM olp.requests r
 LEFT JOIN olp.api_keys k ON k.id=r.api_key_id LEFT JOIN olp.routes rt ON rt.slug=r.route_slug WHERE r.id=NEW.request_id;
 v_outcome := CASE WHEN v_request.error_class IS NULL AND v_request.status_code BETWEEN 200 AND 299 THEN 'success' ELSE 'failure' END;
 PERFORM olp.enqueue_export('requests',v_request.id,v_project,v_request.route_slug,v_outcome,COALESCE(v_request.completed_at,v_request.started_at),to_jsonb(v_request));
 IF jsonb_array_length(v_request.policy_decisions)>0 THEN
  PERFORM olp.enqueue_export('guardrail_decisions',v_request.id,v_project,v_request.route_slug,v_outcome,COALESCE(v_request.completed_at,v_request.started_at),
   jsonb_build_object('request_id',v_request.id,'decisions',v_request.policy_decisions));
 END IF;
 FOR v_attempt IN SELECT a.* FROM olp.attempts a WHERE a.request_id=NEW.request_id ORDER BY a.ordinal LOOP
  PERFORM olp.enqueue_export('attempts',v_attempt.id,v_project,v_request.route_slug,
   CASE WHEN v_attempt.error_class IS NULL AND v_attempt.status_code BETWEEN 200 AND 299 THEN 'success' ELSE 'failure' END,
   COALESCE(v_attempt.completed_at,v_attempt.started_at),to_jsonb(v_attempt));
 END LOOP;
 FOR v_rollup IN
  SELECT md5(jsonb_build_array(NEW.event_id,f.provider_id,f.route_slug,f.upstream_model,f.operation,f.surface,f.api_key_id,
   f.budget_group_id,f.attribution,f.model_family,f.estimate_provenance,f.end_user_digest,f.budget_exempt,f.currency,
   date_trunc('hour',f.observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',olp.budget_bucket(f.observed_at))::text)::uuid AS source_id,
   max(f.observed_at) AS observed_at,
   jsonb_build_object('kind','delta','event_id',NEW.event_id,'bucket',date_trunc('hour',f.observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    'budget_bucket',olp.budget_bucket(f.observed_at),'project_id',v_project,'route',f.route_slug,'provider_id',f.provider_id,
    'upstream_model',f.upstream_model,'operation',f.operation,'surface',f.surface,'api_key_id',f.api_key_id,'budget_group_id',f.budget_group_id,
    'model_family',f.model_family,'estimate_provenance',f.estimate_provenance,
    'attribution',f.attribution,'end_user_digest',f.end_user_digest,'budget_exempt',f.budget_exempt,
    'request_count',sum(f.request_counted::integer),'provider_request_count',sum(f.provider_request_counted::integer),
    'model_request_count',sum(f.model_request_counted::integer),'target_request_count',sum(f.target_request_counted::integer),
    'request_unpriced_count',sum(f.request_unpriced_counted::integer),'provider_unpriced_count',sum(f.provider_unpriced_counted::integer),
    'model_unpriced_count',sum(f.model_unpriced_counted::integer),'target_unpriced_count',sum(f.target_unpriced_counted::integer),
    'request_incomplete_count',sum(f.request_incomplete_counted::integer),'provider_incomplete_count',sum(f.provider_incomplete_counted::integer),
    'model_incomplete_count',sum(f.model_incomplete_counted::integer),'target_incomplete_count',sum(f.target_incomplete_counted::integer),
    'attempt_count',count(*),'input_tokens',sum(f.input_tokens),'output_tokens',sum(f.output_tokens),
    'cached_input_tokens',sum(f.cached_input_tokens),'cache_write_input_tokens',sum(f.cache_write_input_tokens),
    'cache_write_5m_input_tokens',sum(f.cache_write_5m_input_tokens),'cache_write_1h_input_tokens',sum(f.cache_write_1h_input_tokens),
    'audio_input_tokens',sum(f.audio_input_tokens),'cached_audio_input_tokens',sum(f.cached_audio_input_tokens),
    'audio_output_tokens',sum(f.audio_output_tokens),
    'image_input_tokens',sum(f.image_input_tokens),'cached_image_input_tokens',sum(f.cached_image_input_tokens),
    'image_output_tokens',sum(f.image_output_tokens),
    'media_units',sum(f.media_units)::text,'estimated_cost',sum(f.estimated_cost)::text,'currency',f.currency,
    'estimated_input_tokens',sum(CASE WHEN f.usage_observed AND f.input_tokens IS NOT NULL AND f.estimated_input_tokens IS NOT NULL THEN f.estimated_input_tokens ELSE 0 END)::text,
    'estimate_reported_input_tokens',sum(CASE WHEN f.usage_observed AND f.input_tokens IS NOT NULL AND f.estimated_input_tokens IS NOT NULL THEN f.input_tokens ELSE 0 END)::text,
    'estimate_attempt_count',sum(CASE WHEN f.usage_observed AND f.input_tokens IS NOT NULL AND f.estimated_input_tokens IS NOT NULL THEN 1 ELSE 0 END),
    'unpriced_attempt_count',count(*) FILTER(WHERE f.charge_status<>'not_billable' AND f.unpriced),
    'incomplete_attempt_count',count(*) FILTER(WHERE f.charge_status<>'not_billable' AND NOT f.usage_complete)) AS payload
  FROM olp.attempt_usage_facts f WHERE f.event_id=NEW.event_id
  GROUP BY f.provider_id,f.route_slug,f.upstream_model,f.operation,f.surface,f.api_key_id,f.budget_group_id,f.attribution,
   f.model_family,f.estimate_provenance,f.end_user_digest,f.budget_exempt,f.currency,
   date_trunc('hour',f.observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',olp.budget_bucket(f.observed_at)
 LOOP
  PERFORM olp.enqueue_export('usage_rollups',v_rollup.source_id,v_project,v_request.route_slug,v_outcome,v_rollup.observed_at,v_rollup.payload);
 END LOOP;
 RETURN NEW;
END $$;
