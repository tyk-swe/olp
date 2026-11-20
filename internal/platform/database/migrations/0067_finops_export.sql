-- M6.4/M6.5 foundation: exact list and contracted cost on usage facts and
-- hourly rollups, the caller-credential marker, the metering/FOCUS/Stripe sink
-- types, and the tables the FOCUS generator and billing importer keep their
-- delivery state in.
--
-- Facts priced before this migration have NULL list/contracted costs; readers
-- resolve them as estimated_cost + COALESCE(commitment_weight, 0). No backfill
-- is run, so large fact tables are not rewritten.

ALTER TABLE olp.attempt_usage_facts
    ADD COLUMN list_cost numeric(24,12),
    ADD COLUMN contracted_cost numeric(24,12),
    ADD COLUMN caller_credential boolean NOT NULL DEFAULT false;
ALTER TABLE olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_cost_pair CHECK (
        (list_cost IS NULL OR list_cost >= 0)
        AND (contracted_cost IS NULL OR contracted_cost >= 0)
        AND (list_cost IS NULL) = (contracted_cost IS NULL)
        AND (list_cost IS NULL OR estimated_cost IS NOT NULL));

ALTER TABLE olp.attempt_usage_hourly
    ADD COLUMN list_cost numeric(30,12) CHECK (list_cost >= 0),
    ADD COLUMN contracted_cost numeric(30,12) CHECK (contracted_cost >= 0);

ALTER TABLE olp.export_sinks
    DROP CONSTRAINT export_sinks_type_check,
    ADD CONSTRAINT export_sinks_type_check
        CHECK (type IN ('https','otlp_logs','s3','gcs','azure_blob','stripe_meter')),
    DROP CONSTRAINT export_sinks_format_check,
    ADD CONSTRAINT export_sinks_format_check
        CHECK (format IN ('json','jsonl','otlp','cloudevents','csv_gzip','stripe')),
    DROP CONSTRAINT export_sinks_streams_check,
    ADD CONSTRAINT export_sinks_streams_check
        CHECK (cardinality(streams) BETWEEN 1 AND 6
               AND streams <@ ARRAY['requests','attempts','usage_rollups',
                                    'guardrail_decisions','audit','metering','focus']::text[]),
    DROP CONSTRAINT export_sinks_check,
    ADD CONSTRAINT export_sinks_check CHECK (
        (type = 'https' AND format = 'json' AND NOT streams @> ARRAY['focus']::text[])
        OR (type = 'https' AND format = 'cloudevents' AND streams <@ ARRAY['metering']::text[])
        OR (type = 'otlp_logs' AND format = 'otlp' AND NOT streams @> ARRAY['focus']::text[])
        OR (type IN ('s3','gcs','azure_blob') AND format = 'jsonl' AND NOT streams @> ARRAY['focus']::text[])
        OR (type IN ('s3','gcs','azure_blob') AND format = 'csv_gzip' AND streams <@ ARRAY['focus']::text[])
        OR (type = 'stripe_meter' AND format = 'stripe' AND streams <@ ARRAY['metering']::text[])),
    ADD COLUMN settings jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(settings) = 'object' AND octet_length(settings::text) <= 131072);

-- A dirty day is one whose priced facts changed after the day's dataset was
-- last delivered; the version is bumped on every dirtied day so a dataset can
-- tell whether it is current without rescanning the facts.
CREATE SEQUENCE olp.focus_dirty_version;
CREATE TABLE olp.focus_dirty_days (
    day date PRIMARY KEY,
    version bigint NOT NULL
);

-- One row per (sink, charge day): the FOCUS dataset delivery ledger.
CREATE TABLE olp.focus_datasets (
    sink_id uuid NOT NULL REFERENCES olp.export_sinks ON DELETE CASCADE,
    charge_date date NOT NULL,
    revision integer NOT NULL DEFAULT 0 CHECK (revision >= 0),
    dirty_version bigint,
    generated_at timestamptz,
    delivered_at timestamptz,
    row_count bigint,
    part_count integer,
    billed_cost numeric(30,12),
    effective_cost numeric(30,12),
    attempts integer NOT NULL DEFAULT 0,
    failures integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error_code text,
    PRIMARY KEY (sink_id, charge_date)
);
CREATE INDEX focus_datasets_due ON olp.focus_datasets (sink_id, next_attempt_at);

-- One billing import per provider: which vendor cost endpoint to read, how
-- often, and how far it got. The credential is a sealed billing_credential
-- secret, write-only through the management API.
CREATE TABLE olp.provider_billing_imports (
    provider_id uuid PRIMARY KEY REFERENCES olp.providers ON DELETE CASCADE,
    source text NOT NULL CHECK (source IN ('openai_costs','anthropic_cost_report')),
    credential_id uuid REFERENCES olp.secrets,
    scope jsonb NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(scope) = 'object' AND octet_length(scope::text) <= 65536),
    enabled boolean NOT NULL DEFAULT true,
    etag uuid NOT NULL,
    created_by uuid NOT NULL REFERENCES olp.users,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    next_import_at timestamptz NOT NULL DEFAULT now(),
    last_import_at timestamptz,
    last_success_at timestamptz,
    last_error_code text,
    -- consecutive_failures backs the worker's exponential retry off.
    consecutive_failures integer NOT NULL DEFAULT 0,
    -- imported_from / imported_through bound the earliest and latest UTC day a
    -- successful import covered; NULL until one has succeeded.
    imported_from date,
    imported_through date
);

-- What the provider invoiced, at the vendor's line-item grain for one day.
CREATE TABLE olp.provider_invoiced_costs (
    provider_id uuid NOT NULL REFERENCES olp.providers ON DELETE CASCADE,
    day date NOT NULL,
    line_item text NOT NULL CHECK (length(line_item) BETWEEN 1 AND 256),
    currency char(3) NOT NULL
        CHECK (currency = upper(currency) AND btrim(currency) ~ '^[A-Z]{3}$'),
    amount numeric(30,12) NOT NULL,
    source text NOT NULL,
    fetched_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_id, day, line_item)
);

ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check,
    ADD CONSTRAINT secrets_purpose_check CHECK (purpose IN ('oidc_client','oidc_flow','mutation_replay',
        'provider_credential','notification_secret','provider_continuation','media_job_source',
        'provider_grant_refresh','grant_enrollment','mfa_totp','mfa_webauthn','saml_key','saml_flow',
        'sink_credential','mcp_credential','billing_credential'));

ALTER TABLE olp.worker_task_health DROP CONSTRAINT worker_task_health_task_check,
    ADD CONSTRAINT worker_task_health_task_check CHECK (task IN ('request_metadata_consumer','maintenance',
        'cost_reconciliation','request_metadata_gateway_epoch_detection','media_reconciliation',
        'notification_delivery','grant_refresh','health_probes','export_delivery','managed_export_delivery',
        'focus_export','billing_import'));

-- M6.4a: the metering stream. olp.metering_value is the sink-configured amount
-- a priced attempt reports (NULL means "not reportable"); export_sink_accepts
-- holds today's scope and filter rules plus the Stripe customer mapping, so a
-- record is enqueued — and a pending row created — only for sinks that will
-- accept it.
CREATE FUNCTION olp.metering_value(settings jsonb, payload jsonb) RETURNS numeric
LANGUAGE sql IMMUTABLE AS $$
 SELECT CASE COALESCE(settings->>'value','charge')
  WHEN 'charge' THEN (payload->>'charge')::numeric
  WHEN 'cost' THEN (payload->>'cost')::numeric
  WHEN 'input_tokens' THEN (payload->>'input_tokens')::numeric
  WHEN 'output_tokens' THEN (payload->>'output_tokens')::numeric
  WHEN 'total_tokens' THEN CASE
   WHEN payload->>'input_tokens' IS NULL AND payload->>'output_tokens' IS NULL THEN NULL
   ELSE COALESCE((payload->>'input_tokens')::numeric,0)+COALESCE((payload->>'output_tokens')::numeric,0) END
  WHEN 'requests' THEN 1 END
$$;

CREATE FUNCTION olp.export_sink_accepts(s olp.export_sinks, p_stream text, p_project uuid,
    p_route text, p_outcome text, p_payload jsonb) RETURNS boolean
LANGUAGE sql STABLE AS $$
 SELECT (s).enabled AND p_stream=ANY((s).streams)
  AND ((s).project_id IS NULL OR (s).project_id=p_project)
  AND (NOT((s).filter?'project') OR (s).filter->>'project'=p_project::text)
  AND (NOT((s).filter?'route') OR (s).filter->>'route'=p_route)
  AND (NOT((s).filter?'outcome') OR (s).filter->>'outcome'=p_outcome)
  AND ((s).type<>'stripe_meter' OR
       (p_project IS NOT NULL AND (s).settings->'customers' ? p_project::text
        AND olp.metering_value((s).settings,p_payload)>0))
$$;

CREATE OR REPLACE FUNCTION olp.enqueue_export(p_stream text,p_source uuid,p_project uuid,p_route text,p_outcome text,p_occurred timestamptz,p_payload jsonb)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE v_record uuid := gen_random_uuid();
BEGIN
 INSERT INTO olp.export_records(id,stream,source_id,project_id,route,outcome,occurred_at,payload)
 SELECT v_record,p_stream,p_source,p_project,p_route,p_outcome,p_occurred,p_payload
 WHERE EXISTS(SELECT 1 FROM olp.export_sinks s
  WHERE olp.export_sink_accepts(s,p_stream,p_project,p_route,p_outcome,p_payload))
 ON CONFLICT(stream,source_id) DO NOTHING;
 IF NOT FOUND THEN RETURN; END IF;
 INSERT INTO olp.export_pending(sink_id,record_id)
 SELECT s.id,v_record FROM olp.export_sinks s
 WHERE olp.export_sink_accepts(s,p_stream,p_project,p_route,p_outcome,p_payload);
END $$;

-- The metering record is one per priced attempt: billable, complete usage and
-- a provider cost or a consumer charge. The consumer project is the rate-card
-- ownership chain — the key's project, else the provider's, else the route's.
CREATE OR REPLACE FUNCTION olp.export_accounted_request() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v_request olp.requests%ROWTYPE; v_project uuid; v_outcome text; v_attempt record; v_rollup record; v_meter record;
BEGIN
 IF NEW.status<>'fact_persisted' OR OLD.status='fact_persisted' THEN RETURN NEW; END IF;
 IF NOT EXISTS(SELECT 1 FROM olp.export_sinks WHERE enabled AND streams && ARRAY['requests','attempts','usage_rollups','guardrail_decisions','metering','focus']::text[]) THEN RETURN NEW; END IF;
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
 FOR v_meter IN
  SELECT f.attempt_id,f.route_slug,f.observed_at,
   COALESCE(k.project_id,pr.project_id,rt.project_id) AS consumer,
   CASE WHEN a.id IS NULL OR (a.error_class IS NULL AND a.status_code BETWEEN 200 AND 299)
        THEN 'success' ELSE 'failure' END AS attempt_outcome,
   jsonb_build_object(
    'attempt_id',f.attempt_id,'request_id',f.request_id,'observed_at',f.observed_at,
    'project_id',COALESCE(k.project_id,pr.project_id,rt.project_id),
    'api_key_id',f.api_key_id,'route',f.route_slug,'provider_id',f.provider_id,
    'upstream_model',f.upstream_model,'operation',f.operation,'surface',f.surface,
    'input_tokens',f.input_tokens,'output_tokens',f.output_tokens,
    'cached_input_tokens',f.cached_input_tokens,'cache_write_input_tokens',f.cache_write_input_tokens,
    'cache_write_5m_input_tokens',f.cache_write_5m_input_tokens,'cache_write_1h_input_tokens',f.cache_write_1h_input_tokens,
    'audio_input_tokens',f.audio_input_tokens,'cached_audio_input_tokens',f.cached_audio_input_tokens,
    'audio_output_tokens',f.audio_output_tokens,
    'image_input_tokens',f.image_input_tokens,'cached_image_input_tokens',f.cached_image_input_tokens,
    'image_output_tokens',f.image_output_tokens,
    'media_units',f.media_units::text,'tool_calls',f.tool_calls,'batch',f.batch,
    'cost',f.estimated_cost::text,'charge',f.charge::text,'list_cost',f.list_cost::text,
    'currency',btrim(f.currency),'pricing_revision_id',f.pricing_revision_id,
    'rate_card_revision_id',f.rate_card_revision_id,'discount_id',f.discount_id,
    'commitment_weight',f.commitment_weight::text,
    'attribution',f.attribution,'end_user_digest',f.end_user_digest) AS payload
  FROM olp.attempt_usage_facts f
  LEFT JOIN olp.attempts a ON a.id=f.attempt_id
  LEFT JOIN olp.api_keys k ON k.id=f.api_key_id
  LEFT JOIN olp.providers pr ON pr.id=f.provider_id
  LEFT JOIN olp.routes rt ON rt.slug=f.route_slug
  WHERE f.event_id=NEW.event_id AND f.charge_status='billable' AND f.usage_complete
   AND (f.estimated_cost IS NOT NULL OR f.charge IS NOT NULL)
 LOOP
  PERFORM olp.enqueue_export('metering',v_meter.attempt_id,v_meter.consumer,v_meter.route_slug,
   v_meter.attempt_outcome,v_meter.observed_at,v_meter.payload);
 END LOOP;
 -- A focus dataset restates a whole day, so a priced fact dirties its UTC
 -- day. A committed fact also dirties every day of the commitment bucket
 -- that might already hold a delivered revision.
 IF EXISTS(SELECT 1 FROM olp.export_sinks WHERE enabled AND 'focus' = ANY(streams)) THEN
  INSERT INTO olp.focus_dirty_days(day,version)
  SELECT DISTINCT (f.observed_at AT TIME ZONE 'UTC')::date,nextval('olp.focus_dirty_version')
  FROM olp.attempt_usage_facts f WHERE f.event_id=NEW.event_id
  UNION
  SELECT d.day,nextval('olp.focus_dirty_version')
  FROM olp.attempt_usage_facts f
  CROSS JOIN LATERAL (
   SELECT generate_series(
    date_trunc(f.commitment_period,f.observed_at AT TIME ZONE 'UTC')::date,
    least((date_trunc(f.commitment_period,f.observed_at AT TIME ZONE 'UTC')
           + ('1 '||f.commitment_period)::interval)::date - 1,
          (now() AT TIME ZONE 'UTC')::date),
    '1 day'::interval)::date AS day
  ) d WHERE f.event_id=NEW.event_id AND f.commitment_period IS NOT NULL
  ON CONFLICT (day) DO UPDATE SET version=nextval('olp.focus_dirty_version');
 END IF;
 FOR v_rollup IN
  SELECT md5(jsonb_build_array(NEW.event_id,f.provider_id,f.route_slug,f.upstream_model,f.operation,f.surface,f.api_key_id,
   f.budget_group_id,f.attribution,f.model_family,f.estimate_provenance,f.end_user_digest,f.budget_exempt,f.currency,
   f.commitment_amount,f.commitment_period,
   date_trunc('hour',f.observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',olp.budget_bucket(f.observed_at))::text)::uuid AS source_id,
   max(f.observed_at) AS observed_at,
   jsonb_build_object('kind','delta','event_id',NEW.event_id,'bucket',date_trunc('hour',f.observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
    'budget_bucket',olp.budget_bucket(f.observed_at),'project_id',v_project,'route',f.route_slug,'provider_id',f.provider_id,
    'upstream_model',f.upstream_model,'operation',f.operation,'surface',f.surface,'api_key_id',f.api_key_id,'budget_group_id',f.budget_group_id,
    'model_family',f.model_family,'estimate_provenance',f.estimate_provenance,
    'attribution',f.attribution,'end_user_digest',f.end_user_digest,'budget_exempt',f.budget_exempt,
    'commitment_amount',f.commitment_amount::text,'commitment_period',f.commitment_period,
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
    'commitment_weight',sum(f.commitment_weight)::text) ||
   jsonb_build_object('charge',sum(f.charge)::text,
    'uncharged_attempt_count',count(*) FILTER(WHERE f.charge_status<>'not_billable' AND f.charge IS NULL),
    'estimated_input_tokens',sum(CASE WHEN f.usage_observed AND f.input_tokens IS NOT NULL AND f.estimated_input_tokens IS NOT NULL THEN f.estimated_input_tokens ELSE 0 END)::text,
    'estimate_reported_input_tokens',sum(CASE WHEN f.usage_observed AND f.input_tokens IS NOT NULL AND f.estimated_input_tokens IS NOT NULL THEN f.input_tokens ELSE 0 END)::text,
    'estimate_attempt_count',sum(CASE WHEN f.usage_observed AND f.input_tokens IS NOT NULL AND f.estimated_input_tokens IS NOT NULL THEN 1 ELSE 0 END),
    'unpriced_attempt_count',count(*) FILTER(WHERE f.charge_status<>'not_billable' AND f.unpriced),
    'incomplete_attempt_count',count(*) FILTER(WHERE f.charge_status<>'not_billable' AND NOT f.usage_complete)) AS payload
  FROM olp.attempt_usage_facts f WHERE f.event_id=NEW.event_id
  GROUP BY f.provider_id,f.route_slug,f.upstream_model,f.operation,f.surface,f.api_key_id,f.budget_group_id,f.attribution,
   f.model_family,f.estimate_provenance,f.end_user_digest,f.budget_exempt,f.currency,f.commitment_amount,f.commitment_period,
   date_trunc('hour',f.observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',olp.budget_bucket(f.observed_at)
 LOOP
  PERFORM olp.enqueue_export('usage_rollups',v_rollup.source_id,v_project,v_request.route_slug,v_outcome,v_rollup.observed_at,v_rollup.payload);
 END LOOP;
 RETURN NEW;
END $$;

-- M6.4b price lines: a priced attempt's cost and charge decompose into SKU
-- price lines. olp.price_usage_lines walks the same pricing rules as
-- olp.price_usage and returns NULL exactly where that function returns
-- incomplete; every number is an exact decimal rendered as a string.
CREATE FUNCTION olp.price_usage_lines(price jsonb, usage jsonb, started_at timestamptz,
                                successful boolean, discount numeric DEFAULT 1)
RETURNS jsonb
LANGUAGE plpgsql STABLE AS $$
DECLARE
    rates jsonb := price;
    tier jsonb;
    v_tier text;
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
    v_multiplier numeric := 1;
    v_base jsonb := '[]'::jsonb;
    v_result jsonb;
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
        RETURN NULL;
    END IF;
    -- A tier replaces the whole per-million rate set for the attempt whose
    -- input exceeds its threshold; the highest exceeded threshold wins.
    IF v_input IS NOT NULL AND jsonb_typeof(price->'tiers') = 'array' THEN
        SELECT t INTO tier FROM jsonb_array_elements(price->'tiers') AS t
        WHERE (t->>'above_input_tokens')::bigint < v_input
        ORDER BY (t->>'above_input_tokens')::bigint DESC LIMIT 1;
        IF FOUND THEN
            rates := tier;
            v_tier := tier->>'above_input_tokens';
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
            ELSIF share.tokens > 0 THEN
                v_base := v_base || jsonb_build_object('component', share_field, 'tier', v_tier,
                    'unit', '1000000 Tokens', 'quantity', (share.tokens::numeric / 1000000)::text,
                    'base', share_rate::text);
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
            ELSIF share.tokens > 0 THEN
                v_base := v_base || jsonb_build_object('component', share_field, 'tier', v_tier,
                    'unit', '1000000 Tokens', 'quantity', (share.tokens::numeric / 1000000)::text,
                    'base', share_rate::text);
            END IF;
        END LOOP;
    END IF;

    -- Media, per-request and tool lines bill from the entry itself: a tier
    -- never rewrites them, so their tier member stays null.
    IF v_media IS NOT NULL THEN
        IF price->>'unit_price' IS NULL THEN
            v_ok := false;
        ELSE
            IF v_media > 0 THEN
                v_base := v_base || jsonb_build_object('component', 'unit_price', 'tier', NULL,
                    'unit', 'Units', 'quantity', v_media::text,
                    'base', (price->>'unit_price')::numeric::text);
            END IF;
        END IF;
    END IF;

    IF successful AND price->>'per_request' IS NOT NULL THEN
        v_base := v_base || jsonb_build_object('component', 'per_request', 'tier', NULL,
            'unit', 'Requests', 'quantity', '1',
            'base', (price->>'per_request')::numeric::text);
    END IF;

    IF jsonb_typeof(usage->'tool_calls') = 'object' THEN
        FOR tool IN SELECT key, value::bigint AS calls
            FROM jsonb_each_text(usage->'tool_calls') WHERE value::bigint > 0
        LOOP
            fee := (price->'tool_calls'->>tool.key)::numeric;
            IF fee IS NULL THEN
                v_ok := false;
            ELSE
                v_base := v_base || jsonb_build_object('component', 'tool_calls.' || tool.key, 'tier', NULL,
                    'unit', 'Calls', 'quantity', tool.calls::text,
                    'base', fee::text);
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
        END IF;
    END IF;

    IF v_batch THEN
        IF price->>'batch_multiplier' IS NULL THEN
            v_ok := false;
        ELSE
            v_multiplier := v_multiplier * (price->>'batch_multiplier')::numeric;
        END IF;
    END IF;

    v_ok := v_ok AND COALESCE((usage->>'complete')::boolean, false);
    IF NOT v_ok THEN
        RETURN NULL;
    END IF;

    -- Rates become unit prices under the window and batch multipliers and the
    -- consumer discount; equal lines merge on summed quantity, and the result
    -- orders deterministically so equal inputs marshal to identical bytes.
    SELECT jsonb_agg(jsonb_build_object(
               'component', g.component, 'tier', g.tier, 'unit', g.unit,
               'quantity', trim_scale(g.quantity)::text,
               'list_unit_price', g.list_price::text,
               'contracted_unit_price', g.contracted_price::text)
           ORDER BY g.component, g.tier, g.list_price)
    INTO v_result
    FROM (
        SELECT x.component, x.tier, x.unit, sum(x.quantity) AS quantity,
               x.list_price, x.contracted_price
        FROM (
            SELECT l->>'component' AS component, l->>'tier' AS tier, l->>'unit' AS unit,
                   (l->>'quantity')::numeric AS quantity,
                   trim_scale((l->>'base')::numeric * v_multiplier) AS list_price,
                   trim_scale((l->>'base')::numeric * v_multiplier * COALESCE(discount, 1)) AS contracted_price
            FROM jsonb_array_elements(v_base) l
        ) x
        GROUP BY x.component, x.tier, x.unit, x.list_price, x.contracted_price
    ) g;
    RETURN COALESCE(v_result, '[]'::jsonb);
END $$;

-- olp.scale_price_lines reprices stored cost lines to the consumer's charge
-- lines: every unit price becomes the contracted price scaled by the card's
-- factor, so a markup card or a zeroed commitment reprices the same lines.
CREATE FUNCTION olp.scale_price_lines(lines jsonb, factor numeric) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE v_result jsonb;
BEGIN
    IF lines IS NULL OR factor IS NULL THEN
        RETURN NULL;
    END IF;
    SELECT jsonb_agg(jsonb_build_object(
               'component', g.component, 'tier', g.tier, 'unit', g.unit,
               'quantity', trim_scale(g.quantity)::text,
               'list_unit_price', g.price::text,
               'contracted_unit_price', g.price::text)
           ORDER BY g.component, g.tier, g.price)
    INTO v_result
    FROM (
        SELECT x.component, x.tier, x.unit, sum(x.quantity) AS quantity, x.price
        FROM (
            SELECT l->>'component' AS component, l->>'tier' AS tier, l->>'unit' AS unit,
                   (l->>'quantity')::numeric AS quantity,
                   trim_scale((l->>'contracted_unit_price')::numeric * factor) AS price
            FROM jsonb_array_elements(lines) l
        ) x
        GROUP BY x.component, x.tier, x.unit, x.price
    ) g;
    RETURN COALESCE(v_result, '[]'::jsonb);
END $$;

ALTER TABLE olp.attempt_usage_facts
    ADD COLUMN cost_lines jsonb,
    ADD COLUMN charge_lines jsonb,
    ADD CONSTRAINT attempt_usage_facts_cost_lines_check
        CHECK (cost_lines IS NULL OR
               (jsonb_typeof(cost_lines) = 'array' AND octet_length(cost_lines::text) <= 16384
                AND estimated_cost IS NOT NULL)),
    ADD CONSTRAINT attempt_usage_facts_charge_lines_check
        CHECK (charge_lines IS NULL OR
               (jsonb_typeof(charge_lines) = 'array' AND octet_length(charge_lines::text) <= 16384
                AND charge IS NOT NULL));
