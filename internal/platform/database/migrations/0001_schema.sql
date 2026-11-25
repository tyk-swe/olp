SET LOCAL check_function_bodies = false;

CREATE FUNCTION olp.active_budget_increase(owner uuid, kind text, win bigint, at timestamp with time zone) RETURNS numeric
    LANGUAGE sql STABLE
    AS $$
 SELECT COALESCE(sum(amount),0) FROM olp.budget_increases
 WHERE owner_id=owner AND window_kind=kind AND window_id=win AND revoked_at IS NULL AND starts_at<=at AND expires_at>at
$$;

CREATE FUNCTION olp.advance_credential_slot_etag() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF (to_jsonb(NEW)-'etag') IS DISTINCT FROM (to_jsonb(OLD)-'etag') THEN
  NEW.etag := gen_random_uuid();
 END IF;
 RETURN NEW;
END;
$$;

CREATE FUNCTION olp.backfill_commitment_hours(p_limit integer DEFAULT 512, p_cutoff timestamp with time zone DEFAULT NULL::timestamp with time zone) RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE rec record; s olp.commitment_schedules; pid uuid; n integer:=0; consumer uuid; cname text;
BEGIN
 FOR rec IN SELECT h.ctid row_ctid,h.*,md5('commitment-hour/'||to_jsonb(h)::text)::uuid identity
  FROM olp.attempt_usage_hourly h WHERE h.commitment_backfill_pending AND h.commitment_period IS NOT NULL
   AND h.bucket>=olp.commitment_bucket(h.commitment_period,COALESCE(p_cutoff,now()-make_interval(days=>COALESCE(
    (SELECT value::integer FROM olp.settings WHERE key='retention.usage_days'),90))))
   AND NOT EXISTS(SELECT 1 FROM olp.commitment_usage u WHERE u.attempt_id=md5('commitment-hour/'||to_jsonb(h)::text)::uuid)
   AND EXISTS(SELECT 1 FROM olp.commitment_schedules cs WHERE cs.historical AND cs.provider_id=h.provider_id AND cs.period=h.commitment_period
    AND cs.effective_at<olp.commitment_end(h.commitment_period,olp.commitment_bucket(h.commitment_period,h.bucket))
    AND (cs.ends_at IS NULL OR cs.ends_at>h.bucket))
  ORDER BY h.bucket LIMIT p_limit FOR UPDATE OF h SKIP LOCKED
 LOOP
  SELECT * INTO s FROM olp.commitment_schedules cs WHERE cs.provider_id=rec.provider_id AND cs.period=rec.commitment_period
   AND cs.effective_at<olp.commitment_end(rec.commitment_period,olp.commitment_bucket(rec.commitment_period,rec.bucket))
   AND (cs.ends_at IS NULL OR cs.ends_at>rec.bucket) ORDER BY cs.effective_at DESC LIMIT 1;
  pid:=olp.ensure_commitment_period(s.id,olp.commitment_bucket(rec.commitment_period,rec.bucket));
  SELECT COALESCE(k.project_id,s.project_id,r.project_id) INTO consumer FROM (SELECT 1) x
   LEFT JOIN olp.api_keys k ON k.id=rec.api_key_id LEFT JOIN olp.routes r ON r.slug=rec.route_slug;
  SELECT name INTO cname FROM olp.projects WHERE id=consumer;
  INSERT INTO olp.commitment_usage(attempt_id,period_id,observed_at,weight,eligible,complete,payload)
  VALUES(rec.identity,pid,rec.bucket,rec.commitment_weight,NOT rec.budget_exempt,false,
   (to_jsonb(rec)-'identity'-'row_ctid')||jsonb_build_object('attempt_id',rec.identity,'observed_at',rec.bucket,'charge_status','billable',
    'usage_complete',true,'caller_credential',false,'commitment_schedule_id',s.id,
    'consumer',consumer,'consumer_name',cname,'connection_name',s.provider_name,
    'provider_kind',s.provider_kind,'vendor_id',s.vendor_id,'provider_project',s.project_id)) ON CONFLICT DO NOTHING;
  UPDATE olp.commitment_periods SET complete=false WHERE id=pid;
  PERFORM olp.dirty_commitment(olp.commitment_bucket(rec.commitment_period,rec.bucket),olp.commitment_end(rec.commitment_period,olp.commitment_bucket(rec.commitment_period,rec.bucket)));
  UPDATE olp.attempt_usage_hourly SET commitment_backfill_pending=false WHERE ctid=rec.row_ctid;
  n:=n+1;
 END LOOP;
 RETURN n;
END $$;

SET default_table_access_method = heap;

CREATE TABLE olp.batch_pending_subjects (
    resource_id uuid NOT NULL,
    ordinal integer NOT NULL,
    api_key_id uuid NOT NULL,
    budget_group_id uuid,
    project_id uuid,
    api_key_project_id uuid,
    route_id uuid,
    leg_route_id uuid,
    pinned_route_id uuid,
    budget_owner_ids uuid[] NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL,
    slot_id uuid,
    attribution jsonb NOT NULL,
    end_user_digest text,
    CONSTRAINT batch_pending_subjects_ordinal_check CHECK ((ordinal > 0))
);

CREATE TABLE olp.batch_settlements (
    resource_id uuid NOT NULL,
    kind text NOT NULL,
    adapter text NOT NULL,
    api_key_id uuid NOT NULL,
    route_slug text NOT NULL,
    route_revision_id uuid NOT NULL,
    provider_id uuid NOT NULL,
    provider_revision_id uuid NOT NULL,
    target_id text NOT NULL,
    slot_id uuid NOT NULL,
    credential_id uuid,
    upstream_id text,
    surface text NOT NULL,
    submission_digest bytea NOT NULL,
    line_operation text NOT NULL,
    template jsonb NOT NULL,
    state text NOT NULL,
    expected_results bigint,
    settled_results bigint DEFAULT 0 NOT NULL,
    uncertain_results bigint DEFAULT 0 NOT NULL,
    cursor jsonb DEFAULT '{}'::jsonb NOT NULL,
    claim_id uuid,
    claimed_until timestamp with time zone,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error_class text,
    close_after timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    closed_at timestamp with time zone,
    submission jsonb,
    submission_source text,
    creator_deadline timestamp with time zone NOT NULL,
    CONSTRAINT batch_settlements_adapter_check CHECK ((adapter = ANY (ARRAY['openai'::text, 'together'::text, 'mistral'::text, 'gemini_managed'::text, 'anthropic'::text, 'gemini'::text, 'vertex'::text, 'bedrock'::text]))),
    CONSTRAINT batch_settlements_check CHECK (((claim_id IS NULL) = (claimed_until IS NULL))),
    CONSTRAINT batch_settlements_check1 CHECK (((state = ANY (ARRAY['settled'::text, 'uncertain'::text])) = (closed_at IS NOT NULL))),
    CONSTRAINT batch_settlements_expected_results_check CHECK ((expected_results >= 0)),
    CONSTRAINT batch_settlements_last_error_class_check CHECK (((last_error_class IS NULL) OR (octet_length(last_error_class) <= 120))),
    CONSTRAINT batch_settlements_line_operation_check CHECK ((line_operation = ANY (ARRAY['generation'::text, 'embeddings'::text]))),
    CONSTRAINT batch_settlements_state_check CHECK ((state = ANY (ARRAY['submitting'::text, 'submission_unknown'::text, 'active'::text, 'settling'::text, 'settled'::text, 'uncertain'::text, 'conflict'::text]))),
    CONSTRAINT batch_settlements_surface_check CHECK ((surface = ANY (ARRAY['openai'::text, 'anthropic'::text, 'gemini'::text, 'native'::text]))),
    CONSTRAINT batch_settlements_upstream_id_check CHECK (((upstream_id IS NULL) OR ((octet_length(upstream_id) >= 1) AND (octet_length(upstream_id) <= 512))))
);

CREATE FUNCTION olp.batch_settlement_subjects(bs olp.batch_settlements) RETURNS SETOF olp.batch_pending_subjects
    LANGUAGE sql STABLE
    AS $$
    SELECT bs.resource_id,
           (a->>'ordinal')::integer,
           bs.api_key_id,
           NULLIF(bs.template->>'budget_group_id', '')::uuid,
           COALESCE(k.project_id, p.project_id, r.project_id),
           k.project_id,
           r.id,
           leg_r.id,
           pinned_r.id,
           ARRAY(
               SELECT b::uuid
               FROM jsonb_array_elements_text(COALESCE(a->'routing'->'budgets', '[]'::jsonb)) b
           ),
           bs.template->>'route_slug',
           (a->>'provider_id')::uuid,
           NULLIF(a->'routing'->>'credential_slot_id', '')::uuid,
           COALESCE(bs.template->'attribution', '{}'::jsonb),
           NULLIF(bs.template->>'end_user_digest', '')
    FROM jsonb_array_elements(
        CASE WHEN jsonb_typeof(bs.template->'attempts') = 'array'
             THEN bs.template->'attempts' ELSE '[]'::jsonb END
    ) a
    JOIN olp.api_keys k ON k.id = bs.api_key_id
    JOIN olp.providers p ON p.id = (a->>'provider_id')::uuid
    LEFT JOIN olp.routes r ON r.slug = bs.template->>'route_slug'
    LEFT JOIN olp.routes pinned_r ON pinned_r.slug = bs.route_slug
    LEFT JOIN olp.routes leg_r ON leg_r.slug = a->'routing'->'leg'->>'route'
    WHERE NOT COALESCE((a->'routing'->>'budget_exempt')::boolean, false);
$$;

CREATE FUNCTION olp.budget_allowances(owner uuid, effective jsonb, budget jsonb) RETURNS jsonb
    LANGUAGE plpgsql STABLE
    AS $$
DECLARE kind text; field text; cap numeric; extra numeric; expiry timestamptz; win bigint; detail jsonb;
BEGIN
 FOR kind,field IN SELECT * FROM (VALUES('day','daily'),('week','weekly'),('month','monthly')) v(k,f) LOOP
  cap:=(effective->>(field||'_cost_limit'))::numeric;
  SELECT window_id INTO win FROM olp.budget_window(kind,now());
  SELECT COALESCE(sum(amount),0),min(expires_at) INTO extra,expiry FROM olp.budget_increases
   WHERE owner_id=owner AND window_kind=kind AND window_id=win AND revoked_at IS NULL AND starts_at<=now() AND expires_at>now() AND cap IS NOT NULL;
  detail:=COALESCE(budget->field,'{}'::jsonb)||jsonb_build_object('effective_limit',(cap+extra)::text,'temporary_increase',extra::text,'increase_expires_at',expiry);
  IF detail ? 'remaining' THEN
   detail:=detail||jsonb_build_object('remaining',CASE WHEN cap IS NULL THEN NULL ELSE GREATEST(cap+extra-(detail->>'accrued')::numeric,0)::text END);
  END IF;
  budget:=jsonb_set(budget,ARRAY[field],detail);
 END LOOP;
 RETURN budget;
END $$;

CREATE FUNCTION olp.budget_bucket(instant timestamp with time zone) RETURNS timestamp with time zone
    LANGUAGE sql STABLE STRICT
    AS $$
 SELECT GREATEST(date_trunc('hour',instant AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',daily_start,weekly_start,monthly_start)
 FROM olp.budget_windows(instant)
$$;

CREATE FUNCTION olp.budget_calendar_status(instant timestamp with time zone) RETURNS jsonb
    LANGUAGE sql STABLE
    AS $$
 SELECT jsonb_agg(jsonb_build_object('window_kind',k.kind,'time_zone',w.time_zone,'starts_at',w.start_at,'ends_at',w.end_at,
 'pending_time_zone',c.time_zone,'effective_at',c.effective_at) ORDER BY k.ordinal)
 FROM unnest(ARRAY['day','week','month']) WITH ORDINALITY k(kind,ordinal)
 CROSS JOIN LATERAL olp.budget_window(k.kind,instant) w
 LEFT JOIN LATERAL (SELECT time_zone,effective_at FROM olp.budget_calendar_changes WHERE window_kind=k.kind AND effective_at>instant ORDER BY effective_at LIMIT 1) c ON true
$$;

CREATE FUNCTION olp.budget_civil_boundary(civil_date date, zone_name text) RETURNS timestamp with time zone
    LANGUAGE plpgsql STABLE STRICT
    AS $$
DECLARE low bigint; high bigint; middle bigint;
BEGIN
 IF zone_name='UTC' THEN RETURN civil_date::timestamp AT TIME ZONE 'UTC'; END IF;
 low:=extract(epoch FROM civil_date::timestamp AT TIME ZONE 'UTC')::bigint-172800;
 high:=low+345600;
 WHILE low<high LOOP
  middle:=low+(high-low)/2;
  IF (to_timestamp(middle) AT TIME ZONE zone_name)::date<civil_date THEN low:=middle+1;
  ELSE high:=middle; END IF;
 END LOOP;
 RETURN to_timestamp(low);
END $$;

CREATE FUNCTION olp.budget_window(kind text, instant timestamp with time zone) RETURNS TABLE(window_id bigint, start_at timestamp with time zone, end_at timestamp with time zone, time_zone text)
    LANGUAGE plpgsql STABLE STRICT
    AS $$
DECLARE change_at timestamptz; next_change timestamptz; civil_date date; next_date date;
BEGIN
 IF kind NOT IN ('day','week','month') THEN RAISE EXCEPTION 'invalid budget window'; END IF;
 SELECT c.effective_at,c.time_zone INTO STRICT change_at,time_zone
 FROM olp.budget_calendar_changes c WHERE c.window_kind=kind AND c.effective_at<=instant
 ORDER BY c.effective_at DESC LIMIT 1;
 SELECT min(c.effective_at) INTO next_change FROM olp.budget_calendar_changes c
 WHERE c.window_kind=kind AND c.effective_at>instant;
 civil_date:=date_trunc(kind,instant AT TIME ZONE time_zone)::date;
 next_date:=CASE kind WHEN 'day' THEN civil_date+1 WHEN 'week' THEN civil_date+7 ELSE (civil_date+interval '1 month')::date END;
 start_at:=GREATEST(olp.budget_civil_boundary(civil_date,time_zone),change_at);
 end_at:=LEAST(olp.budget_civil_boundary(next_date,time_zone),next_change);
 -- Preserve pre-feature UTC identifiers. Subsequent windows use their exact
 -- start instant, which is monotone and cannot collide with legacy labels.
 IF change_at='1970-01-01 00:00:00+00'::timestamptz THEN
  window_id:=CASE kind WHEN 'day' THEN floor(extract(epoch FROM start_at)/86400)::bigint
   WHEN 'week' THEN floor((floor(extract(epoch FROM start_at)/86400)+3)/7)::bigint
   ELSE extract(year FROM civil_date)::bigint*12+extract(month FROM civil_date)::bigint-1 END;
 ELSE window_id:=extract(epoch FROM start_at)::bigint; END IF;
 RETURN NEXT;
END $$;

CREATE FUNCTION olp.budget_windows(instant timestamp with time zone) RETURNS TABLE(daily_id bigint, daily_start timestamp with time zone, daily_end timestamp with time zone, monthly_id bigint, monthly_start timestamp with time zone, monthly_end timestamp with time zone, weekly_id bigint, weekly_start timestamp with time zone, weekly_end timestamp with time zone)
    LANGUAGE sql STABLE STRICT ROWS 1
    AS $$
 SELECT d.window_id,d.start_at,d.end_at,m.window_id,m.start_at,m.end_at,w.window_id,w.start_at,w.end_at
 FROM olp.budget_window('day',instant) d CROSS JOIN olp.budget_window('month',instant) m CROSS JOIN olp.budget_window('week',instant) w
$$;

CREATE FUNCTION olp.capture_commitment_usage() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE s olp.commitment_schedules; pid uuid; started timestamptz; consumer uuid; cname text; doc jsonb;
BEGIN
 SELECT COALESCE(a.started_at,NEW.observed_at) INTO started FROM olp.attempts a WHERE a.id=NEW.attempt_id;
 started:=COALESCE(started,NEW.observed_at);
 SELECT * INTO s FROM olp.commitment_schedules WHERE id=NEW.commitment_schedule_id;
 IF s.id IS NULL THEN
  SELECT * INTO s FROM olp.commitment_schedules WHERE provider_id=NEW.provider_id AND effective_at<=started
   AND (ends_at IS NULL OR ends_at>started) ORDER BY effective_at DESC LIMIT 1;
 END IF;
 IF s.id IS NULL THEN RETURN NEW; END IF;
 NEW.commitment_schedule_id:=s.id;
 NEW.commitment_bucket:=COALESCE(NEW.commitment_bucket,olp.commitment_bucket(s.period,started));
 pid:=olp.ensure_commitment_period(s.id,NEW.commitment_bucket);
 SELECT COALESCE(k.project_id,s.project_id,r.project_id) INTO consumer
 FROM (SELECT 1) x LEFT JOIN olp.api_keys k ON k.id=NEW.api_key_id LEFT JOIN olp.routes r ON r.slug=NEW.route_slug;
 SELECT name INTO cname FROM olp.projects WHERE id=consumer;
 doc:=olp.commitment_fact_payload(NEW)||jsonb_build_object('consumer',consumer,'consumer_name',cname,
  'connection_name',s.provider_name,'provider_kind',s.provider_kind,'vendor_id',s.vendor_id,'provider_project',s.project_id);
 INSERT INTO olp.commitment_usage(attempt_id,period_id,observed_at,weight,eligible,complete,payload)
 VALUES(NEW.attempt_id,pid,NEW.observed_at,COALESCE(NEW.commitment_weight,0),
  NOT NEW.caller_credential AND NOT NEW.budget_exempt AND NEW.charge_status='billable' AND NEW.usage_complete AND NEW.estimated_cost IS NOT NULL,
  NEW.charge_status='not_billable' OR (NEW.usage_complete AND NOT NEW.unpriced),doc)
 ON CONFLICT(attempt_id) DO UPDATE SET weight=EXCLUDED.weight,eligible=EXCLUDED.eligible,complete=EXCLUDED.complete,payload=EXCLUDED.payload;
 PERFORM olp.dirty_commitment(NEW.commitment_bucket,olp.commitment_end(s.period,NEW.commitment_bucket));
 RETURN NEW;
END $$;

CREATE FUNCTION olp.check_approval_dispatch() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF NEW.consumed_at IS NOT NULL AND NOT EXISTS(
 SELECT 1 FROM olp.execution_debits d WHERE d.id=NEW.dispatch_id AND d.run_id=NEW.run_id
 AND d.api_key_id=NEW.api_key_id AND d.project_id=NEW.project_id
 AND d.state IN ('dispatched','outcome_unknown','settled','conflict','not_accepted'))
 THEN RAISE EXCEPTION 'approval must commit with its dispatch'; END IF;
 RETURN NULL;
END $$;

CREATE FUNCTION olp.check_route_revision_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
    IF TG_OP = 'UPDATE' AND (
        NEW.route_id IS DISTINCT FROM OLD.route_id OR
        NEW.slug IS DISTINCT FROM OLD.slug OR
        NEW.fidelity IS DISTINCT FROM OLD.fidelity
    ) THEN
        RAISE EXCEPTION 'published route revision identity is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'route_revision_identity';
    END IF;
    IF NEW.slug IS DISTINCT FROM (SELECT slug FROM olp.routes WHERE id = NEW.route_id) THEN
        RAISE EXCEPTION 'route revision slug must match its route'
            USING ERRCODE = '23514', CONSTRAINT = 'route_revision_identity';
    END IF;
    RETURN NEW;
END $$;

CREATE TABLE olp.code_accounts (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    provider_id uuid NOT NULL,
    credential_id uuid NOT NULL,
    principal text NOT NULL,
    name text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    models jsonb NOT NULL,
    allowance jsonb,
    health text DEFAULT 'unknown'::text NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    unavailable_until timestamp with time zone,
    health_observed_at timestamp with time zone,
    extra_usage_enabled boolean,
    cycle_anchor date,
    allowance_read_requested_at timestamp with time zone,
    allowance_read_at timestamp with time zone,
    max_concurrency integer,
    draining boolean DEFAULT false NOT NULL,
    CONSTRAINT code_accounts_health_check CHECK ((health = ANY (ARRAY['unknown'::text, 'healthy'::text, 'unavailable'::text, 'quota_limited'::text]))),
    CONSTRAINT code_accounts_max_concurrency_check CHECK ((max_concurrency > 0))
);

CREATE FUNCTION olp.code_account_available(a olp.code_accounts) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT (a.health NOT IN ('unavailable','quota_limited') OR coalesce(a.unavailable_until<=now(),false))
      AND NOT EXISTS (
        SELECT 1 FROM (VALUES
          ('remaining_tokens', 'token_observation'),
          ('remaining_requests', 'request_observation')
        ) AS counts(remaining, observation)
        WHERE (a.allowance->>counts.remaining)::bigint=0
          AND NOT coalesce((CASE WHEN a.allowance ? counts.observation
            THEN a.allowance->counts.observation->>'resets_at'
            ELSE a.allowance->>'resets_at' END)::timestamptz<=now(),false))
      AND NOT (
        coalesce((a.allowance->>'remaining_percent')::numeric=0,false)
        AND NOT EXISTS (
          SELECT 1 FROM jsonb_array_elements(coalesce(a.allowance->'windows','[]'::jsonb)) w
          WHERE w->>'limit_id'='codex' AND w->>'window'='primary')
        AND NOT coalesce((a.allowance->>'resets_at')::timestamptz<=now(),false))
      AND NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(coalesce(a.allowance->'windows','[]'::jsonb)) w
        WHERE coalesce(w->>'evidence','passive')='passive'
          AND (CASE coalesce(w->>'unit','percent')
            WHEN 'percent' THEN (w->>'remaining_percent')::numeric=0
            WHEN 'count' THEN (w->>'remaining_value')::numeric=0
            WHEN 'credits' THEN (w->>'remaining_value')::numeric=0
            WHEN 'cents' THEN (w->>'remaining_value')::numeric=0
            ELSE false END)
          AND NOT coalesce((w->>'resets_at')::timestamptz<=now(),false))
$$;

CREATE FUNCTION olp.code_initial_recovery_safe(root uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT EXISTS(SELECT 1 FROM olp.code_bindings b WHERE b.id=root AND b.root_id=b.id AND b.parent_id IS NULL AND b.retired_at IS NULL AND NOT b.socket_bound)
      AND (SELECT count(*) FROM olp.code_bindings WHERE root_id=root)=1
      AND (SELECT count(*) FROM olp.code_pins WHERE root_id=root)=1
      AND EXISTS(SELECT 1 FROM olp.code_attempts a WHERE a.binding_id=root AND a.initial_recovery AND a.nonexecution AND a.state='settled' AND a.reported_tokens=0)
      AND NOT EXISTS(SELECT 1 FROM olp.code_attempts WHERE binding_id=root
          AND (state NOT IN ('settled','aborted') OR state='settled' AND reported_tokens IS NULL))
$$;

CREATE FUNCTION olp.code_recovery_chain(source uuid) RETURNS TABLE(id uuid, account_id uuid, nonexecution boolean)
    LANGUAGE sql STABLE
    AS $$
    WITH RECURSIVE chain AS (
        SELECT a.id,a.recovery_from,a.account_id,a.nonexecution FROM olp.code_attempts a WHERE a.id=source
        UNION ALL SELECT prior.id,prior.recovery_from,prior.account_id,prior.nonexecution FROM olp.code_attempts prior JOIN chain ON prior.id=chain.recovery_from)
    SELECT chain.id,chain.account_id,chain.nonexecution FROM chain
$$;

CREATE FUNCTION olp.commitment_bucket(p_period text, p_at timestamp with time zone) RETURNS timestamp with time zone
    LANGUAGE sql IMMUTABLE STRICT
    AS $$ SELECT date_trunc(p_period,p_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' $$;

CREATE FUNCTION olp.commitment_end(p_period text, p_at timestamp with time zone) RETURNS timestamp with time zone
    LANGUAGE sql IMMUTABLE STRICT
    AS $$ SELECT ((p_at AT TIME ZONE 'UTC')+('1 '||p_period)::interval) AT TIME ZONE 'UTC' $$;

CREATE TABLE olp.attempt_usage_facts (
    attempt_id uuid NOT NULL,
    event_id uuid NOT NULL,
    request_id uuid NOT NULL,
    request_started_at timestamp with time zone NOT NULL,
    attempt_ordinal smallint NOT NULL,
    api_key_id uuid,
    provider_id uuid NOT NULL,
    route_slug text NOT NULL,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    charge_status text NOT NULL,
    usage_observed boolean NOT NULL,
    usage_complete boolean NOT NULL,
    input_tokens bigint,
    output_tokens bigint,
    cached_input_tokens bigint,
    media_units numeric(24,6),
    estimated_cost numeric(24,12),
    unpriced boolean NOT NULL,
    pricing_revision_id uuid,
    currency character(3),
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
    budget_group_id uuid,
    cache_write_input_tokens bigint,
    cache_write_5m_input_tokens bigint,
    cache_write_1h_input_tokens bigint,
    attribution jsonb DEFAULT '{}'::jsonb NOT NULL,
    estimated_input_tokens bigint,
    estimate_provenance text DEFAULT ''::text NOT NULL,
    model_family text DEFAULT ''::text NOT NULL,
    selector text,
    baseline_cost numeric(28,12),
    end_user_digest text DEFAULT ''::text NOT NULL,
    budget_exempt boolean DEFAULT false NOT NULL,
    audio_input_tokens bigint,
    cached_audio_input_tokens bigint,
    audio_output_tokens bigint,
    image_input_tokens bigint,
    cached_image_input_tokens bigint,
    image_output_tokens bigint,
    tool_calls jsonb DEFAULT '{}'::jsonb NOT NULL,
    batch boolean DEFAULT false NOT NULL,
    pricing_components text[],
    commitment_weight numeric(24,12),
    commitment_amount numeric(24,12),
    commitment_period text,
    discount_id uuid,
    charge numeric(24,12),
    rate_card_revision_id uuid,
    list_cost numeric(24,12),
    contracted_cost numeric(24,12),
    caller_credential boolean DEFAULT false NOT NULL,
    cost_lines jsonb,
    charge_lines jsonb,
    response_cache boolean DEFAULT false NOT NULL,
    cache_avoided_cost numeric(24,12) DEFAULT 0 NOT NULL,
    cache_budget_cost numeric(24,12) DEFAULT 0 NOT NULL,
    cache_budget_charge numeric(24,12) DEFAULT 0 NOT NULL,
    commitment_schedule_id uuid,
    commitment_bucket timestamp with time zone,
    training_tokens bigint,
    storage_byte_seconds numeric,
    cache_token_seconds numeric,
    container_sessions jsonb,
    invocation_count bigint,
    agent_principal_id uuid,
    delegation_pair_digest text,
    member_principal_id uuid,
    unpriced_cause text,
    CONSTRAINT attempt_delegation_member CHECK (((member_principal_id IS NULL) OR (agent_principal_id IS NOT NULL))),
    CONSTRAINT attempt_delegation_pair CHECK ((((agent_principal_id IS NULL) AND (delegation_pair_digest IS NULL)) OR ((agent_principal_id IS NOT NULL) AND (delegation_pair_digest IS NOT NULL) AND (delegation_pair_digest ~ '^[0-9a-f]{64}$'::text)))),
    CONSTRAINT attempt_usage_cache_detail_subset CHECK ((((cache_write_5m_input_tokens IS NULL) AND (cache_write_1h_input_tokens IS NULL)) OR ((cache_write_input_tokens IS NOT NULL) AND ((COALESCE(cache_write_5m_input_tokens, (0)::bigint) + COALESCE(cache_write_1h_input_tokens, (0)::bigint)) <= cache_write_input_tokens)))),
    CONSTRAINT attempt_usage_cache_subset CHECK ((((cached_input_tokens IS NULL) AND (cache_write_input_tokens IS NULL)) OR ((input_tokens IS NOT NULL) AND ((COALESCE(cached_input_tokens, (0)::bigint) + COALESCE(cache_write_input_tokens, (0)::bigint)) <= input_tokens)))),
    CONSTRAINT attempt_usage_facts_attempt_ordinal_check CHECK ((attempt_ordinal > 0)),
    CONSTRAINT attempt_usage_facts_attribution_check CHECK (((jsonb_typeof(attribution) = 'object'::text) AND (octet_length((attribution)::text) <= 1024))),
    CONSTRAINT attempt_usage_facts_audio_input_tokens_check CHECK (((audio_input_tokens IS NULL) OR (audio_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_audio_output_tokens_check CHECK (((audio_output_tokens IS NULL) OR (audio_output_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_baseline_check CHECK (((baseline_cost IS NULL) OR (selector IS NOT NULL))),
    CONSTRAINT attempt_usage_facts_baseline_cost_check CHECK ((baseline_cost >= (0)::numeric)),
    CONSTRAINT attempt_usage_facts_billing_uncertain_check CHECK (((charge_status <> 'billing_uncertain'::text) OR (NOT usage_complete))),
    CONSTRAINT attempt_usage_facts_cache_avoided_cost_check CHECK ((cache_avoided_cost >= (0)::numeric)),
    CONSTRAINT attempt_usage_facts_cache_budget_charge_check CHECK ((cache_budget_charge >= (0)::numeric)),
    CONSTRAINT attempt_usage_facts_cache_budget_cost_check CHECK ((cache_budget_cost >= (0)::numeric)),
    CONSTRAINT attempt_usage_facts_cache_token_seconds_check CHECK ((cache_token_seconds >= (0)::numeric)),
    CONSTRAINT attempt_usage_facts_cache_write_1h_input_tokens_check CHECK (((cache_write_1h_input_tokens IS NULL) OR (cache_write_1h_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_cache_write_5m_input_tokens_check CHECK (((cache_write_5m_input_tokens IS NULL) OR (cache_write_5m_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_cache_write_input_tokens_check CHECK (((cache_write_input_tokens IS NULL) OR (cache_write_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_cached_audio_input_tokens_check CHECK (((cached_audio_input_tokens IS NULL) OR (cached_audio_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_cached_image_input_tokens_check CHECK (((cached_image_input_tokens IS NULL) OR (cached_image_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_cached_input_tokens_check CHECK (((cached_input_tokens IS NULL) OR (cached_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_charge CHECK ((((charge IS NULL) OR ((charge_status <> 'not_billable'::text) AND usage_complete)) AND ((discount_id IS NULL) OR (estimated_cost IS NOT NULL)))),
    CONSTRAINT attempt_usage_facts_charge_check CHECK (((charge IS NULL) OR (charge >= (0)::numeric))),
    CONSTRAINT attempt_usage_facts_charge_lines_check CHECK (((charge_lines IS NULL) OR ((jsonb_typeof(charge_lines) = 'array'::text) AND (octet_length((charge_lines)::text) <= 16384) AND (charge IS NOT NULL)))),
    CONSTRAINT attempt_usage_facts_charge_status_check CHECK ((charge_status = ANY (ARRAY['not_billable'::text, 'billable'::text, 'billing_uncertain'::text]))),
    CONSTRAINT attempt_usage_facts_commitment CHECK ((((commitment_weight IS NULL) = (commitment_amount IS NULL)) AND ((commitment_weight IS NULL) = (commitment_period IS NULL)) AND ((commitment_weight IS NULL) OR (estimated_cost = (0)::numeric)))),
    CONSTRAINT attempt_usage_facts_commitment_period_check CHECK ((commitment_period = ANY (ARRAY['hour'::text, 'day'::text, 'month'::text]))),
    CONSTRAINT attempt_usage_facts_cost_lines_check CHECK (((cost_lines IS NULL) OR ((jsonb_typeof(cost_lines) = 'array'::text) AND (octet_length((cost_lines)::text) <= 16384) AND (estimated_cost IS NOT NULL)))),
    CONSTRAINT attempt_usage_facts_cost_pair CHECK ((((list_cost IS NULL) OR (list_cost >= (0)::numeric)) AND ((contracted_cost IS NULL) OR (contracted_cost >= (0)::numeric)) AND ((list_cost IS NULL) = (contracted_cost IS NULL)) AND ((list_cost IS NULL) OR (estimated_cost IS NOT NULL)))),
    CONSTRAINT attempt_usage_facts_currency_check CHECK (((currency IS NULL) OR (((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text)))),
    CONSTRAINT attempt_usage_facts_end_user_digest_check CHECK (((end_user_digest = ''::text) OR (end_user_digest ~ '^[0-9a-f]{64}$'::text))),
    CONSTRAINT attempt_usage_facts_estimate_check CHECK (((estimated_input_tokens IS NULL) = (estimate_provenance = ''::text))),
    CONSTRAINT attempt_usage_facts_estimate_provenance_check CHECK ((estimate_provenance = ANY (ARRAY[''::text, 'tokenizer'::text, 'calibrated'::text, 'heuristic'::text]))),
    CONSTRAINT attempt_usage_facts_estimated_cost_check CHECK (((estimated_cost IS NULL) OR (estimated_cost >= (0)::numeric))),
    CONSTRAINT attempt_usage_facts_estimated_input_tokens_check CHECK (((estimated_input_tokens IS NULL) OR (estimated_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_image_input_tokens_check CHECK (((image_input_tokens IS NULL) OR (image_input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_image_output_tokens_check CHECK (((image_output_tokens IS NULL) OR (image_output_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_input_tokens_check CHECK (((input_tokens IS NULL) OR (input_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_invocation_count_check CHECK ((invocation_count >= 0)),
    CONSTRAINT attempt_usage_facts_media_units_check CHECK (((media_units IS NULL) OR (media_units >= (0)::numeric))),
    CONSTRAINT attempt_usage_facts_model_family_check CHECK (((model_family = ''::text) OR (model_family ~ '^[a-z][a-z0-9_-]{0,63}$'::text))),
    CONSTRAINT attempt_usage_facts_not_billable_check CHECK (((charge_status <> 'not_billable'::text) OR ((NOT usage_observed) AND usage_complete AND (NOT unpriced) AND (estimated_cost IS NULL) AND (pricing_revision_id IS NULL)))),
    CONSTRAINT attempt_usage_facts_output_tokens_check CHECK (((output_tokens IS NULL) OR (output_tokens >= 0))),
    CONSTRAINT attempt_usage_facts_priced_check CHECK (((estimated_cost IS NULL) OR ((charge_status = 'billable'::text) AND usage_complete AND (NOT unpriced)))),
    CONSTRAINT attempt_usage_facts_selector_check CHECK ((selector ~ '^[a-z0-9][a-z0-9-]{0,62}$'::text)),
    CONSTRAINT attempt_usage_facts_storage_byte_seconds_check CHECK ((storage_byte_seconds >= (0)::numeric)),
    CONSTRAINT attempt_usage_facts_surface_check CHECK ((surface ~ '^[a-z][a-z0-9_-]{0,95}$'::text)),
    CONSTRAINT attempt_usage_facts_tool_calls_check CHECK (((jsonb_typeof(tool_calls) = 'object'::text) AND (octet_length((tool_calls)::text) <= 1024))),
    CONSTRAINT attempt_usage_facts_training_tokens_check CHECK ((training_tokens >= 0)),
    CONSTRAINT attempt_usage_facts_unpriced_cause_check CHECK ((unpriced_cause = ANY (ARRAY['no_applicable_price'::text, 'missing_price_component'::text, 'incomplete_usage'::text]))),
    CONSTRAINT attempt_usage_modality_subset CHECK ((((input_tokens IS NOT NULL) OR ((audio_input_tokens IS NULL) AND (cached_audio_input_tokens IS NULL) AND (image_input_tokens IS NULL) AND (cached_image_input_tokens IS NULL))) AND ((output_tokens IS NOT NULL) OR ((audio_output_tokens IS NULL) AND (image_output_tokens IS NULL))) AND (COALESCE(cached_audio_input_tokens, (0)::bigint) <= COALESCE(audio_input_tokens, (0)::bigint)) AND (COALESCE(cached_image_input_tokens, (0)::bigint) <= COALESCE(image_input_tokens, (0)::bigint)) AND ((COALESCE(cached_audio_input_tokens, (0)::bigint) + COALESCE(cached_image_input_tokens, (0)::bigint)) <= COALESCE(cached_input_tokens, (0)::bigint)) AND ((input_tokens IS NULL) OR ((((COALESCE(cached_input_tokens, (0)::bigint) + COALESCE(cache_write_input_tokens, (0)::bigint)) + (COALESCE(audio_input_tokens, (0)::bigint) - COALESCE(cached_audio_input_tokens, (0)::bigint))) + (COALESCE(image_input_tokens, (0)::bigint) - COALESCE(cached_image_input_tokens, (0)::bigint))) <= input_tokens)) AND ((output_tokens IS NULL) OR ((COALESCE(audio_output_tokens, (0)::bigint) + COALESCE(image_output_tokens, (0)::bigint)) <= output_tokens)))),
    CONSTRAINT retained_container_sessions_object CHECK (((container_sessions IS NULL) OR (jsonb_typeof(container_sessions) = 'object'::text)))
);

CREATE FUNCTION olp.commitment_fact_payload(f olp.attempt_usage_facts) RETURNS jsonb
    LANGUAGE sql IMMUTABLE
    AS $$ SELECT jsonb_build_object(
 'attempt_id',(f).attempt_id,
 'provider_id',(f).provider_id,
 'route_slug',(f).route_slug,
 'api_key_id',(f).api_key_id,
 'upstream_model',(f).upstream_model,
 'operation',(f).operation,
 'surface',(f).surface,
 'attribution',(f).attribution,
 'budget_group_id',(f).budget_group_id,
 'end_user_digest',(f).end_user_digest,
 'model_family',(f).model_family,
 'estimate_provenance',(f).estimate_provenance,
 'budget_exempt',(f).budget_exempt,
 'observed_at',(f).observed_at,
 'commitment_weight',(f).commitment_weight,
 'commitment_amount',(f).commitment_amount,
 'commitment_period',(f).commitment_period,
 'commitment_schedule_id',(f).commitment_schedule_id,
 'commitment_bucket',(f).commitment_bucket,
 'list_cost',(f).list_cost,
 'contracted_cost',(f).contracted_cost,
 'estimated_cost',(f).estimated_cost,
 'currency',(f).currency,
 'charge',(f).charge,
 'rate_card_revision_id',(f).rate_card_revision_id,
 'discount_id',(f).discount_id,
 'pricing_revision_id',(f).pricing_revision_id,
 'usage_complete',(f).usage_complete,
 'charge_status',(f).charge_status,
 'unpriced',(f).unpriced,
 'caller_credential',(f).caller_credential,
 'cost_lines',(f).cost_lines) $$;

CREATE TABLE olp.attempt_usage_hourly (
    bucket timestamp with time zone NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL,
    upstream_model text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    api_key_id uuid,
    request_count bigint NOT NULL,
    provider_request_count bigint NOT NULL,
    model_request_count bigint NOT NULL,
    target_request_count bigint NOT NULL,
    input_tokens numeric(30,0) NOT NULL,
    output_tokens numeric(30,0) NOT NULL,
    cached_input_tokens numeric(30,0) NOT NULL,
    media_units numeric(30,6) NOT NULL,
    estimated_cost numeric(30,12),
    request_unpriced_count bigint NOT NULL,
    provider_unpriced_count bigint NOT NULL,
    model_unpriced_count bigint NOT NULL,
    target_unpriced_count bigint NOT NULL,
    request_incomplete_count bigint NOT NULL,
    provider_incomplete_count bigint NOT NULL,
    model_incomplete_count bigint NOT NULL,
    target_incomplete_count bigint NOT NULL,
    currency character(3),
    unpriced_attempt_count bigint DEFAULT 0 NOT NULL,
    budget_group_id uuid,
    cache_write_input_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    cache_write_5m_input_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    cache_write_1h_input_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    attribution jsonb DEFAULT '{}'::jsonb NOT NULL,
    estimate_provenance text DEFAULT ''::text NOT NULL,
    model_family text DEFAULT ''::text NOT NULL,
    estimated_input_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    estimate_reported_input_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    estimate_attempt_count bigint DEFAULT 0 NOT NULL,
    end_user_digest text DEFAULT ''::text NOT NULL,
    budget_bucket timestamp with time zone NOT NULL,
    budget_exempt boolean DEFAULT false NOT NULL,
    audio_input_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    cached_audio_input_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    audio_output_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    image_input_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    cached_image_input_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    image_output_tokens numeric(30,0) DEFAULT 0 NOT NULL,
    commitment_weight numeric(30,12) DEFAULT 0 NOT NULL,
    commitment_amount numeric(24,12),
    commitment_period text,
    charge numeric(30,12),
    uncharged_attempt_count bigint DEFAULT 0 NOT NULL,
    list_cost numeric(30,12),
    contracted_cost numeric(30,12),
    cache_budget_cost numeric(24,12) DEFAULT 0 NOT NULL,
    cache_budget_charge numeric(24,12) DEFAULT 0 NOT NULL,
    provider_cache_hit_count bigint DEFAULT 0 NOT NULL,
    commitment_backfill_pending boolean DEFAULT false NOT NULL,
    training_tokens numeric DEFAULT 0 NOT NULL,
    storage_byte_seconds numeric DEFAULT 0 NOT NULL,
    cache_token_seconds numeric DEFAULT 0 NOT NULL,
    container_sessions jsonb DEFAULT '{}'::jsonb NOT NULL,
    agent_principal_id uuid,
    delegation_pair_digest text DEFAULT ''::text NOT NULL,
    focus_cost_detail_missing boolean DEFAULT true NOT NULL,
    focus_charge_detail_missing boolean DEFAULT true NOT NULL,
    no_applicable_price_count bigint DEFAULT 0 NOT NULL,
    missing_price_component_count bigint DEFAULT 0 NOT NULL,
    incomplete_usage_unpriced_count bigint DEFAULT 0 NOT NULL,
    caller_credential boolean DEFAULT false NOT NULL,
    reconcil_caller_attempts bigint,
    CONSTRAINT attempt_usage_hourly_attribution_check CHECK (((jsonb_typeof(attribution) = 'object'::text) AND (octet_length((attribution)::text) <= 1024))),
    CONSTRAINT attempt_usage_hourly_audio_input_tokens_check CHECK ((audio_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_audio_output_tokens_check CHECK ((audio_output_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_cache_budget_charge_check CHECK ((cache_budget_charge >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_cache_budget_cost_check CHECK ((cache_budget_cost >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_cache_token_seconds_check CHECK ((cache_token_seconds >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_cache_write_1h_input_tokens_check CHECK ((cache_write_1h_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_cache_write_5m_input_tokens_check CHECK ((cache_write_5m_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_cache_write_input_tokens_check CHECK ((cache_write_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_cached_audio_input_tokens_check CHECK ((cached_audio_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_cached_image_input_tokens_check CHECK ((cached_image_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_cached_input_tokens_check CHECK ((cached_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_check CHECK (((provider_cache_hit_count >= 0) AND (provider_cache_hit_count <= provider_request_count))),
    CONSTRAINT attempt_usage_hourly_commitment_period_check CHECK ((commitment_period = ANY (ARRAY['hour'::text, 'day'::text, 'month'::text]))),
    CONSTRAINT attempt_usage_hourly_contracted_cost_check CHECK ((contracted_cost >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_currency_check CHECK (((currency IS NULL) OR (((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text)))),
    CONSTRAINT attempt_usage_hourly_end_user_digest_check CHECK (((end_user_digest = ''::text) OR (end_user_digest ~ '^[0-9a-f]{64}$'::text))),
    CONSTRAINT attempt_usage_hourly_estimate_attempt_count_check CHECK ((estimate_attempt_count >= 0)),
    CONSTRAINT attempt_usage_hourly_estimate_provenance_check CHECK ((estimate_provenance = ANY (ARRAY[''::text, 'tokenizer'::text, 'calibrated'::text, 'heuristic'::text]))),
    CONSTRAINT attempt_usage_hourly_estimate_reported_input_tokens_check CHECK ((estimate_reported_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_estimated_cost_check CHECK ((estimated_cost >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_estimated_input_tokens_check CHECK ((estimated_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_image_input_tokens_check CHECK ((image_input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_image_output_tokens_check CHECK ((image_output_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_input_tokens_check CHECK ((input_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_list_cost_check CHECK ((list_cost >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_media_units_check CHECK ((media_units >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_model_family_check CHECK (((model_family = ''::text) OR (model_family ~ '^[a-z][a-z0-9_-]{0,63}$'::text))),
    CONSTRAINT attempt_usage_hourly_model_incomplete_count_check CHECK ((model_incomplete_count >= 0)),
    CONSTRAINT attempt_usage_hourly_model_request_count_check CHECK ((model_request_count >= 0)),
    CONSTRAINT attempt_usage_hourly_model_unpriced_count_check CHECK ((model_unpriced_count >= 0)),
    CONSTRAINT attempt_usage_hourly_output_tokens_check CHECK ((output_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_provider_incomplete_count_check CHECK ((provider_incomplete_count >= 0)),
    CONSTRAINT attempt_usage_hourly_provider_request_count_check CHECK ((provider_request_count >= 0)),
    CONSTRAINT attempt_usage_hourly_provider_unpriced_count_check CHECK ((provider_unpriced_count >= 0)),
    CONSTRAINT attempt_usage_hourly_request_count_check CHECK ((request_count >= 0)),
    CONSTRAINT attempt_usage_hourly_request_incomplete_count_check CHECK ((request_incomplete_count >= 0)),
    CONSTRAINT attempt_usage_hourly_request_unpriced_count_check CHECK ((request_unpriced_count >= 0)),
    CONSTRAINT attempt_usage_hourly_storage_byte_seconds_check CHECK ((storage_byte_seconds >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_surface_check CHECK ((surface ~ '^[a-z][a-z0-9_-]{0,95}$'::text)),
    CONSTRAINT attempt_usage_hourly_target_incomplete_count_check CHECK ((target_incomplete_count >= 0)),
    CONSTRAINT attempt_usage_hourly_target_request_count_check CHECK ((target_request_count >= 0)),
    CONSTRAINT attempt_usage_hourly_target_unpriced_count_check CHECK ((target_unpriced_count >= 0)),
    CONSTRAINT attempt_usage_hourly_training_tokens_check CHECK ((training_tokens >= (0)::numeric)),
    CONSTRAINT attempt_usage_hourly_unpriced_attempt_count_check CHECK ((unpriced_attempt_count >= 0))
);

CREATE FUNCTION olp.commitment_hourly_allocation(h olp.attempt_usage_hourly) RETURNS numeric
    LANGUAGE sql STABLE
    AS $$
 SELECT SUM(COALESCE(a.allocated_cost,0)) FROM olp.commitment_usage u JOIN olp.commitment_periods cp ON cp.id=u.period_id AND NOT cp.legacy LEFT JOIN olp.commitment_allocations a ON a.attempt_id=u.attempt_id
 WHERE u.observed_at >= (h).bucket AND u.observed_at < (h).bucket+interval '1 hour'
 AND NOT EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE f.attempt_id=u.attempt_id)
 AND u.payload->>'provider_id' IS NOT DISTINCT FROM (h).provider_id::text
 AND u.payload->>'route_slug' IS NOT DISTINCT FROM (h).route_slug
 AND u.payload->>'upstream_model' IS NOT DISTINCT FROM (h).upstream_model
 AND u.payload->>'operation' IS NOT DISTINCT FROM (h).operation
 AND u.payload->>'surface' IS NOT DISTINCT FROM (h).surface
 AND u.payload->>'api_key_id' IS NOT DISTINCT FROM (h).api_key_id::text
 AND u.payload->>'budget_group_id' IS NOT DISTINCT FROM (h).budget_group_id::text
 AND u.payload->'attribution' IS NOT DISTINCT FROM (h).attribution
 AND u.payload->>'end_user_digest' IS NOT DISTINCT FROM (h).end_user_digest
 AND u.payload->>'model_family' IS NOT DISTINCT FROM (h).model_family
 AND u.payload->>'estimate_provenance' IS NOT DISTINCT FROM (h).estimate_provenance
 AND (u.payload->>'budget_exempt')::boolean=(h).budget_exempt
 AND ((h).reconcil_caller_attempts IS NULL
      OR COALESCE((u.payload->>'caller_credential')::boolean,false)=(h).caller_credential)
 AND (u.payload->>'commitment_amount')::numeric IS NOT DISTINCT FROM (h).commitment_amount
 AND u.payload->>'commitment_period' IS NOT DISTINCT FROM (h).commitment_period
$$;

CREATE FUNCTION olp.commitment_period_dirty() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN PERFORM olp.dirty_commitment(NEW.bucket_start,NEW.bucket_end); RETURN NEW; END $$;

CREATE FUNCTION olp.commitment_period_identity() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 SELECT provider_id,period INTO NEW.provider_id,NEW.period FROM olp.commitment_schedules WHERE id=NEW.schedule_id;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.container_session_string_totals(documents jsonb) RETURNS jsonb
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
 SELECT jsonb_object_agg(key,value) FROM jsonb_each_text(olp.container_session_totals(documents));
$$;

CREATE FUNCTION olp.container_session_totals(documents jsonb) RETURNS jsonb
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
 SELECT jsonb_object_agg(tier,quantity) FROM (
 SELECT e.key AS tier,sum(e.value::numeric) AS quantity FROM jsonb_array_elements(documents) d
 CROSS JOIN LATERAL jsonb_each_text(CASE WHEN jsonb_typeof(d)='object' THEN d ELSE '{}'::jsonb END) e GROUP BY e.key) counts;
$$;

CREATE FUNCTION olp.default_budget_bucket() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN NEW.budget_bucket:=COALESCE(NEW.budget_bucket,NEW.bucket); RETURN NEW; END $$;

CREATE FUNCTION olp.dirty_commitment(p_start timestamp with time zone, p_end timestamp with time zone) RETURNS void
    LANGUAGE sql
    AS $$
 INSERT INTO olp.focus_dirty_days(day,version)
 SELECT day::date,nextval('olp.focus_dirty_version') FROM generate_series(
  p_start AT TIME ZONE 'UTC', (p_end-interval '1 microsecond') AT TIME ZONE 'UTC',interval '1 day') day
 ON CONFLICT(day) DO UPDATE SET version=EXCLUDED.version
$$;

CREATE FUNCTION olp.dirty_commitment_observed_days() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE first_day timestamptz; after_last_day timestamptz;
BEGIN
 SELECT date_trunc('day',bucket_start AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
  (date_trunc('day',(bucket_end-interval '1 microsecond') AT TIME ZONE 'UTC')+interval '1 day') AT TIME ZONE 'UTC'
 INTO first_day,after_last_day FROM olp.commitment_periods WHERE id=NEW.period_id;
 IF NOT EXISTS(SELECT 1 FROM olp.commitment_usage WHERE period_id=NEW.period_id AND observed_at<first_day)
  AND NOT EXISTS(SELECT 1 FROM olp.commitment_usage WHERE period_id=NEW.period_id AND observed_at>=after_last_day)
 THEN RETURN NEW; END IF;
 INSERT INTO olp.focus_dirty_days(day,version)
 SELECT affected.day,nextval('olp.focus_dirty_version') FROM (
  SELECT day::date FROM generate_series(first_day AT TIME ZONE 'UTC',
   (after_last_day-interval '1 microsecond') AT TIME ZONE 'UTC',interval '1 day') day
  UNION
  SELECT (observed_at AT TIME ZONE 'UTC')::date FROM olp.commitment_usage
   WHERE period_id=NEW.period_id AND observed_at<first_day
  UNION
  SELECT (observed_at AT TIME ZONE 'UTC')::date FROM olp.commitment_usage
   WHERE period_id=NEW.period_id AND observed_at>=after_last_day
 ) affected ORDER BY affected.day
 ON CONFLICT(day) DO UPDATE SET version=EXCLUDED.version;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.effective_limits(local_limits jsonb, template_limits jsonb) RETURNS jsonb
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
 SELECT jsonb_build_object(
 'requests_per_minute',LEAST((local_limits->>'requests_per_minute')::bigint,(template_limits->>'requests_per_minute')::bigint),
 'tokens_per_minute',LEAST((local_limits->>'tokens_per_minute')::bigint,(template_limits->>'tokens_per_minute')::bigint),
 'max_concurrency',LEAST((local_limits->>'max_concurrency')::bigint,(template_limits->>'max_concurrency')::bigint),
 'daily_cost_limit',LEAST((local_limits->>'daily_cost_limit')::numeric,(template_limits->>'daily_cost_limit')::numeric)::text,
 'weekly_cost_limit',LEAST((local_limits->>'weekly_cost_limit')::numeric,(template_limits->>'weekly_cost_limit')::numeric)::text,
 'monthly_cost_limit',LEAST((local_limits->>'monthly_cost_limit')::numeric,(template_limits->>'monthly_cost_limit')::numeric)::text,
 'cost_basis',COALESCE(local_limits->>'cost_basis',template_limits->>'cost_basis','cost'));
$$;

CREATE FUNCTION olp.enforce_media_job_lifecycle_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.lifecycle_state = 'deleted' AND NEW.lifecycle_state <> 'deleted' THEN
        RAISE EXCEPTION 'deleted media job lifecycle is terminal'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'creating'
       AND NEW.lifecycle_state NOT IN (
           'creating', 'active', 'create_ambiguous', 'create_cleanup_pending', 'deleted'
       )
    THEN
        RAISE EXCEPTION 'invalid creating media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'active'
       AND NEW.lifecycle_state NOT IN ('active', 'delete_pending')
    THEN
        RAISE EXCEPTION 'invalid active media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'create_ambiguous'
       AND NEW.lifecycle_state NOT IN (
           'create_ambiguous', 'create_cleanup_pending', 'deleted'
       )
    THEN
        RAISE EXCEPTION 'invalid ambiguous media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'create_cleanup_pending'
       AND NEW.lifecycle_state NOT IN ('create_cleanup_pending', 'deleted')
    THEN
        RAISE EXCEPTION 'invalid create cleanup media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.lifecycle_state = 'delete_pending'
       AND NEW.lifecycle_state NOT IN ('delete_pending', 'deleted')
    THEN
        RAISE EXCEPTION 'invalid delete-pending media job lifecycle transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.lifecycle_state = 'deleted' THEN
        NEW.deleted_at := COALESCE(NEW.deleted_at, now());
        NEW.content_available := false;
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION olp.enforce_media_job_transition() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF OLD.state IN ('succeeded', 'failed', 'cancelled') AND NEW.state <> OLD.state THEN
        RAISE EXCEPTION 'terminal media job state cannot transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'queued' AND NEW.state NOT IN ('queued', 'running', 'succeeded', 'failed', 'cancelled') THEN
        RAISE EXCEPTION 'invalid queued media job transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.state = 'running' AND NEW.state NOT IN ('running', 'succeeded', 'failed', 'cancelled') THEN
        RAISE EXCEPTION 'invalid running media job transition'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.progress_percent IS NOT NULL
       AND OLD.progress_percent IS NOT NULL
       AND NEW.progress_percent < OLD.progress_percent
    THEN
        RAISE EXCEPTION 'media job progress cannot decrease'
            USING ERRCODE = 'check_violation';
    END IF;
    NEW.updated_at := now();
    IF NEW.state IN ('succeeded', 'failed', 'cancelled') THEN
        NEW.completed_at := COALESCE(NEW.completed_at, now());
    END IF;
    RETURN NEW;
END;
$$;

CREATE FUNCTION olp.enqueue_export(p_stream text, p_source uuid, p_project uuid, p_route text, p_outcome text, p_occurred timestamp with time zone, p_payload jsonb) RETURNS void
    LANGUAGE plpgsql
    AS $$
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

CREATE FUNCTION olp.enqueue_managed_export(stream_name text, owning_project uuid, body jsonb) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE event uuid := gen_random_uuid();
BEGIN
    INSERT INTO olp.managed_export_deliveries(sink_id,event_id,stream,payload)
    SELECT id,event,stream_name,body FROM olp.managed_export_sinks
    WHERE enabled AND retired_at IS NULL AND stream_name=ANY(streams)
      AND (project_id IS NULL OR project_id=owning_project);
END;
$$;

CREATE FUNCTION olp.enqueue_managed_workflow_evidence() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
 stream_name text;
 body jsonb;
 event uuid := gen_random_uuid();
BEGIN
 IF TG_TABLE_NAME='agent_runs' THEN
  stream_name := 'runs';
  body := jsonb_build_object('evidence_origin','gateway_observed','id',NEW.id,
   'project_id',NEW.project_id,'api_key_id',NEW.api_key_id,'created_at',NEW.created_at);
 ELSE
  stream_name := 'trace_spans';
  body := jsonb_build_object('evidence_origin','client_asserted','id',NEW.id,
   'project_id',NEW.project_id,'api_key_id',NEW.api_key_id,
   'trace_id',encode(NEW.trace_id,'hex'),'span_id',encode(NEW.span_id,'hex'),
   'parent_span_id',encode(NEW.parent_span_id,'hex'),
   'run_id',NEW.run_id,'attempt_id',NEW.attempt_id,'started_nanos',NEW.started_nanos::text,
   'ended_nanos',NEW.ended_nanos::text,'category',NEW.category,'status',NEW.status,
   'attributes',NEW.attributes,'received_at',NEW.received_at);
 END IF;
 INSERT INTO olp.managed_export_deliveries(sink_id,event_id,stream,payload,evidence_source_id,evidence_project_id)
 SELECT id,event,stream_name,body,NEW.id,NEW.project_id FROM olp.managed_export_sinks
 WHERE enabled AND retired_at IS NULL AND (project_id IS NULL OR project_id=NEW.project_id) AND stream_name=ANY(streams);
 RETURN NEW;
END $$;

CREATE FUNCTION olp.ensure_commitment_period(p_schedule uuid, p_bucket timestamp with time zone) RETURNS uuid
    LANGUAGE plpgsql
    AS $$
DECLARE result uuid;
BEGIN
 INSERT INTO olp.commitment_periods(schedule_id,bucket_start,bucket_end,amount,complete,legacy)
 SELECT id,p_bucket,olp.commitment_end(period,p_bucket),
 CASE WHEN historical AND p_bucket<(SELECT at FROM olp.commitment_cutover) THEN COALESCE((
 SELECT e.amount FROM (
  SELECT f.commitment_amount amount,f.observed_at at FROM olp.attempt_usage_facts f
  WHERE f.provider_id=cs.provider_id AND f.commitment_period=cs.period AND f.observed_at>=p_bucket AND f.observed_at<olp.commitment_end(cs.period,p_bucket)
  UNION ALL SELECT h.commitment_amount,h.bucket FROM olp.attempt_usage_hourly h
  WHERE h.provider_id=cs.provider_id AND h.commitment_period=cs.period AND h.bucket>=p_bucket AND h.bucket<olp.commitment_end(cs.period,p_bucket)
 ) e WHERE e.amount IS NOT NULL ORDER BY e.at DESC,e.amount DESC LIMIT 1),amount) ELSE amount END,
  NOT (historical AND p_bucket < (SELECT at FROM olp.commitment_cutover)),
  historical AND p_bucket < (SELECT at FROM olp.commitment_cutover)
 FROM olp.commitment_schedules cs WHERE id=p_schedule
 ON CONFLICT(provider_id,period,bucket_start) DO NOTHING;
 SELECT p.id INTO result FROM olp.commitment_periods p JOIN olp.commitment_schedules s ON s.id=p_schedule
 WHERE p.provider_id=s.provider_id AND p.period=s.period AND p.bucket_start=p_bucket;
 RETURN result;
END $$;

CREATE FUNCTION olp.export_accounted_request() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
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
 IF jsonb_array_length(v_request.guardrail_decisions)>0 THEN
  PERFORM olp.enqueue_export('guardrail_decisions',v_request.id,v_project,v_request.route_slug,v_outcome,COALESCE(v_request.completed_at,v_request.started_at),
   jsonb_build_object('request_id',v_request.id,'decisions',v_request.guardrail_decisions));
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
    'invocation_count',f.invocation_count,'training_tokens',f.training_tokens,'storage_byte_seconds',f.storage_byte_seconds::text,'cache_token_seconds',f.cache_token_seconds::text,'container_sessions',f.container_sessions,
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
 IF EXISTS(SELECT 1 FROM olp.export_sinks WHERE enabled AND 'focus' = ANY(streams)) THEN
  INSERT INTO olp.focus_dirty_days(day,version)
  SELECT dirty.day,nextval('olp.focus_dirty_version')
  FROM (
   SELECT (f.observed_at AT TIME ZONE 'UTC')::date AS day
   FROM olp.attempt_usage_facts f WHERE f.event_id=NEW.event_id
   UNION
   SELECT d.day
   FROM olp.attempt_usage_facts f
   CROSS JOIN LATERAL (
    SELECT generate_series(
     date_trunc(f.commitment_period,f.observed_at AT TIME ZONE 'UTC')::date,
     least((date_trunc(f.commitment_period,f.observed_at AT TIME ZONE 'UTC')
            + ('1 '||f.commitment_period)::interval)::date - 1,
           (now() AT TIME ZONE 'UTC')::date),
     '1 day'::interval)::date AS day
   ) d WHERE f.event_id=NEW.event_id AND f.commitment_period IS NOT NULL
  ) dirty ORDER BY dirty.day
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
   jsonb_build_object('training_tokens',sum(f.training_tokens),'storage_byte_seconds',sum(f.storage_byte_seconds)::text,'cache_token_seconds',sum(f.cache_token_seconds)::text,'container_sessions',olp.container_session_totals(jsonb_agg(f.container_sessions))) ||
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

CREATE FUNCTION olp.export_attempt_metadata() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE owning_project uuid;
BEGIN
    SELECT project_id INTO owning_project FROM olp.api_keys WHERE id=NEW.api_key_id;
    PERFORM olp.enqueue_managed_export('attempts',owning_project,jsonb_build_object(
      'attempt_id',NEW.attempt_id,'request_id',NEW.request_id,'route',NEW.route_slug,
      'operation',NEW.operation,'surface',NEW.surface,'observed_at',NEW.observed_at,
      'charge_status',NEW.charge_status,'usage_complete',NEW.usage_complete,
      'input_tokens',NEW.input_tokens,'output_tokens',NEW.output_tokens,'cached_input_tokens',NEW.cached_input_tokens,
      'cost',NEW.estimated_cost::text,'currency',NEW.currency,'unpriced',NEW.unpriced));
    RETURN NEW;
END;
$$;

CREATE FUNCTION olp.export_audit_metadata() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM olp.enqueue_managed_export('audit',NULL,jsonb_build_object('audit_id',NEW.id,
      'action',NEW.action,'resource_type',NEW.resource_type,'outcome',NEW.outcome,'occurred_at',NEW.occurred_at));
    RETURN NEW;
END;
$$;

CREATE FUNCTION olp.export_audit_record() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 PERFORM olp.enqueue_export('audit',NEW.id,NEW.project_id,'',NEW.outcome,NEW.occurred_at,to_jsonb(NEW));
 RETURN NEW;
END $$;

CREATE FUNCTION olp.export_client_trace_span() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE route_slug text;
BEGIN
 SELECT revision.slug INTO route_slug FROM olp.agent_runs run
 JOIN olp.route_revisions revision ON revision.id = run.route_revision_id
 WHERE run.id = NEW.run_id;
 PERFORM olp.enqueue_export('trace_spans', NEW.id, NEW.project_id, COALESCE(route_slug, ''), '', NEW.received_at,
  jsonb_build_object('id', NEW.id, 'project_id', NEW.project_id,
   'trace_id', encode(NEW.trace_id, 'hex'), 'span_id', encode(NEW.span_id, 'hex'),
   'parent_span_id', encode(NEW.parent_span_id, 'hex'),
   'run_id', NEW.run_id, 'attempt_id', NEW.attempt_id, 'started_nanos', NEW.started_nanos,
   'ended_nanos', NEW.ended_nanos, 'category', NEW.category, 'status', NEW.status,
   'attributes', NEW.attributes, 'evidence_origin', 'client_asserted'));
 RETURN NEW;
END $$;

CREATE FUNCTION olp.export_request_metadata() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE owning_project uuid; body jsonb;
BEGIN
    SELECT project_id INTO owning_project FROM olp.api_keys WHERE id=NEW.api_key_id;
    body := jsonb_build_object('request_id',NEW.id,'route',NEW.route_slug,'operation',NEW.operation,
      'surface',NEW.surface,'started_at',NEW.started_at,'completed_at',NEW.completed_at,
      'status_code',NEW.status_code,'error_class',NEW.error_class,'latency_ms',NEW.total_latency_ms,
      'first_byte_ms',NEW.first_byte_ms,'attempt_count',NEW.attempt_count,'origin',NEW.origin);
    PERFORM olp.enqueue_managed_export('requests',owning_project,body);
    IF jsonb_array_length(NEW.guardrail_decisions)>0 THEN
      PERFORM olp.enqueue_managed_export('guardrail_decisions',owning_project,jsonb_build_object('request_id',NEW.id,'decisions',NEW.guardrail_decisions));
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE olp.export_sinks (
    id uuid NOT NULL,
    name text NOT NULL,
    project_id uuid,
    type text NOT NULL,
    destination text NOT NULL,
    credential_id uuid,
    streams text[] NOT NULL,
    format text NOT NULL,
    filter jsonb DEFAULT '{}'::jsonb NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    settings jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT export_sinks_check CHECK ((((type = 'https'::text) AND (format = 'json'::text) AND (NOT (streams @> ARRAY['focus'::text]))) OR ((type = 'https'::text) AND (format = 'cloudevents'::text) AND (streams <@ ARRAY['metering'::text])) OR ((type = 'otlp_logs'::text) AND (format = 'otlp'::text) AND (NOT (streams @> ARRAY['focus'::text]))) OR ((type = ANY (ARRAY['s3'::text, 'gcs'::text, 'azure_blob'::text])) AND (format = 'jsonl'::text) AND (NOT (streams @> ARRAY['focus'::text]))) OR ((type = ANY (ARRAY['s3'::text, 'gcs'::text, 'azure_blob'::text])) AND (format = 'csv_gzip'::text) AND (streams <@ ARRAY['focus'::text])) OR ((type = 'stripe_meter'::text) AND (format = 'stripe'::text) AND (streams <@ ARRAY['metering'::text])))),
    CONSTRAINT export_sinks_destination_check CHECK (((length(destination) >= 1) AND (length(destination) <= 2048))),
    CONSTRAINT export_sinks_filter_check CHECK (((jsonb_typeof(filter) = 'object'::text) AND (octet_length((filter)::text) <= 4096))),
    CONSTRAINT export_sinks_format_check CHECK ((format = ANY (ARRAY['json'::text, 'jsonl'::text, 'otlp'::text, 'cloudevents'::text, 'csv_gzip'::text, 'stripe'::text]))),
    CONSTRAINT export_sinks_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100))),
    CONSTRAINT export_sinks_settings_check CHECK (((jsonb_typeof(settings) = 'object'::text) AND (octet_length((settings)::text) <= 131072))),
    CONSTRAINT export_sinks_streams_check CHECK (((cardinality(streams) >= 1) AND (cardinality(streams) <= 6) AND (streams <@ ARRAY['requests'::text, 'attempts'::text, 'usage_rollups'::text, 'guardrail_decisions'::text, 'audit'::text, 'metering'::text, 'focus'::text, 'runs'::text, 'trace_spans'::text]))),
    CONSTRAINT export_sinks_type_check CHECK ((type = ANY (ARRAY['https'::text, 'otlp_logs'::text, 's3'::text, 'gcs'::text, 'azure_blob'::text, 'stripe_meter'::text])))
);

CREATE FUNCTION olp.export_sink_accepts(s olp.export_sinks, p_stream text, p_project uuid, p_route text, p_outcome text, p_payload jsonb) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
 SELECT (s).enabled AND p_stream=ANY((s).streams)
  AND ((s).project_id IS NULL OR (s).project_id=p_project)
  AND (NOT((s).filter?'project') OR (s).filter->>'project'=p_project::text)
  AND (NOT((s).filter?'route') OR (s).filter->>'route'=p_route)
  AND (NOT((s).filter?'outcome') OR (s).filter->>'outcome'=p_outcome)
  AND ((s).type<>'stripe_meter' OR
       (p_project IS NOT NULL AND (s).settings->'customers' ? p_project::text
        AND olp.metering_value((s).settings,p_payload)>0))
$$;

CREATE FUNCTION olp.export_workflow_run() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE route_slug text;
BEGIN
 SELECT slug INTO route_slug FROM olp.route_revisions WHERE id = NEW.route_revision_id;
 PERFORM olp.enqueue_export('runs', NEW.id, NEW.project_id, COALESCE(route_slug, ''), '', NEW.created_at,
  jsonb_build_object('run_id', NEW.id, 'project_id', NEW.project_id,
   'route_revision_id', NEW.route_revision_id, 'agent_revision_id', NEW.revision_id,
   'created_at', NEW.created_at, 'evidence_origin', 'gateway_observed'));
 RETURN NEW;
END $$;

CREATE FUNCTION olp.guard_agent_approval() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF ROW(NEW.id,NEW.api_key_id,NEW.project_id,NEW.run_id,NEW.binding_digest,NEW.expires_at)
 IS DISTINCT FROM ROW(OLD.id,OLD.api_key_id,OLD.project_id,OLD.run_id,OLD.binding_digest,OLD.expires_at)
 OR OLD.consumed_at IS NOT NULL THEN RAISE EXCEPTION 'approval evidence is immutable'; END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.guard_execution_debit() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF (NEW.id,NEW.api_key_id,NEW.project_id,NEW.run_id,NEW.step_id,NEW.kind,NEW.affinity,NEW.intent_digest,NEW.claim_id,NEW.currency,NEW.cost_bound,NEW.charge_bound,NEW.prepared_until,NEW.created_at)
 IS DISTINCT FROM
 (OLD.id,OLD.api_key_id,OLD.project_id,OLD.run_id,OLD.step_id,OLD.kind,OLD.affinity,OLD.intent_digest,OLD.claim_id,OLD.currency,OLD.cost_bound,OLD.charge_bound,OLD.prepared_until,OLD.created_at)
 THEN RAISE EXCEPTION 'execution debit affinity is immutable'; END IF;
 IF OLD.closed_at IS NOT NULL AND NEW IS DISTINCT FROM OLD THEN RAISE EXCEPTION 'closed execution debit is immutable'; END IF;
 IF OLD.dispatched_at IS NOT NULL AND (NEW.dispatched_at IS DISTINCT FROM OLD.dispatched_at OR NEW.state IN ('prepared','ready','released'))
 THEN RAISE EXCEPTION 'dispatched execution cannot be refunded or replayed'; END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.guard_mcp_tool_call() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'tool call evidence is immutable'; END IF;
 IF (NEW.id,NEW.dispatch_id,NEW.api_key_id,NEW.project_id,NEW.toolset_id,NEW.toolset_revision_id,NEW.server_id,NEW.server_revision_id,NEW.tool_alias,NEW.input_digest,NEW.call_digest,NEW.started_at,NEW.created_at)
 IS DISTINCT FROM
 (OLD.id,OLD.dispatch_id,OLD.api_key_id,OLD.project_id,OLD.toolset_id,OLD.toolset_revision_id,OLD.server_id,OLD.server_revision_id,OLD.tool_alias,OLD.input_digest,OLD.call_digest,OLD.started_at,OLD.created_at)
 THEN RAISE EXCEPTION 'tool call identity is immutable'; END IF;
 IF NOT ((OLD.status='dispatched' AND NEW.status IN ('succeeded','upstream_error','outcome_unknown','guardrail_blocked'))
      OR (OLD.status='outcome_unknown' AND NEW.status IN ('settled','not_accepted','succeeded','upstream_error','guardrail_blocked')))
 THEN RAISE EXCEPTION 'tool call status transition is not permitted'; END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.guard_retained_resource_pin() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF OLD.contract_version='retained-resources-1' AND
 (NEW.kind,NEW.api_key_id,NEW.route_slug,NEW.provider_id,NEW.provider_revision_id,NEW.route_revision_id,NEW.target_id,NEW.slot_id,NEW.credential_id,NEW.parent_id,NEW.contract_version)
 IS DISTINCT FROM
 (OLD.kind,OLD.api_key_id,OLD.route_slug,OLD.provider_id,OLD.provider_revision_id,OLD.route_revision_id,OLD.target_id,OLD.slot_id,OLD.credential_id,OLD.parent_id,OLD.contract_version) THEN
  RAISE EXCEPTION 'retained resource authority is immutable';
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.guard_retained_settlement() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF (NEW.resource_id,NEW.operation,NEW.templates,NEW.token_consuming,NEW.started_at)
   IS DISTINCT FROM (OLD.resource_id,OLD.operation,OLD.templates,OLD.token_consuming,OLD.started_at)
   OR OLD.state='closed' AND NEW.state<>'closed'
   OR OLD.remote_terminated AND NOT NEW.remote_terminated
   OR OLD.terminal_at IS NOT NULL AND NEW.terminal_at IS DISTINCT FROM OLD.terminal_at THEN
  RAISE EXCEPTION 'retained acceptance and settlement evidence is immutable';
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.guardrail_revision_immutable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    RAISE EXCEPTION 'guardrail revisions are immutable' USING ERRCODE = '55000';
END;
$$;

CREATE FUNCTION olp.has_pending_resource_usage(owner uuid) RETURNS boolean
    LANGUAGE sql STABLE
    AS $$
    SELECT olp.has_pending_resource_usage_before_code_paid(owner)
        OR EXISTS(SELECT 1 FROM olp.code_attempts a
            WHERE paid_binding_id IS NOT NULL AND state NOT IN ('prepared','aborted')
                AND paid_cost_owners @> ARRAY[owner]
                AND NOT EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE f.request_id=a.id
                    AND f.usage_complete AND f.estimated_cost IS NOT NULL
                    AND (owner IN (a.paid_provider_id,a.paid_slot_id) OR (f.charge_status<>'billing_uncertain' AND f.charge IS NOT NULL))))
$$;

CREATE FUNCTION olp.has_pending_resource_usage_before_code_paid(owner uuid) RETURNS boolean
    LANGUAGE plpgsql STABLE
    AS $_$
DECLARE
    pending boolean;
    a olp.aggregate_budget_accounts;
    e olp.end_user_accounts;
    projects uuid[];
    column_name text;
BEGIN
    SELECT EXISTS(SELECT 1 FROM olp.api_keys k JOIN olp.provider_resource_pending_usage p ON p.api_key_id=k.id WHERE k.budget_group_id=owner)
     OR EXISTS(SELECT 1 FROM olp.api_keys k JOIN olp.batch_pending_subjects p ON p.api_key_id=k.id WHERE k.budget_group_id=owner) INTO pending;
    IF pending THEN RETURN true; END IF;
    FOREACH column_name IN ARRAY ARRAY[
        'api_key_id', 'budget_group_id', 'provider_id', 'slot_id',
        'route_id', 'pinned_route_id', 'leg_route_id'
    ] LOOP
        EXECUTE format(
            'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE %1$I=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE %1$I=$1)',
            column_name
        ) INTO pending USING owner;
        IF pending THEN RETURN true; END IF;
    END LOOP;
    EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.pending_resource_recorded_owners WHERE budget_owner_id=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_recorded_owners WHERE budget_owner_id=$1)'
        INTO pending USING owner;
    IF pending THEN RETURN true; END IF;
    SELECT * INTO a FROM olp.aggregate_budget_accounts WHERE id = owner;
    IF FOUND THEN
        IF a.installation_id IS NOT NULL THEN
            EXECUTE 'SELECT
                (SELECT resource_id FROM olp.provider_resource_pending_usage ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL
                OR
                (SELECT resource_id FROM olp.batch_pending_subjects ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL'
                INTO pending;
        ELSIF a.organization_id IS NOT NULL THEN
            SELECT array_agg(id) INTO projects FROM olp.projects WHERE organization_id = a.organization_id;
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id = ANY($1))
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = ANY($1))'
                INTO pending USING projects;
        ELSIF a.project_id IS NOT NULL AND a.attribution_key IS NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id = $1)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1)'
                INTO pending USING a.project_id;
        ELSIF a.project_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE project_id = $1 AND attribution @> $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1 AND attribution @> $2)'
                INTO pending USING a.project_id, jsonb_build_object(a.attribution_key, a.attribution_value);
        ELSIF a.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_id = $1 AND route_slug = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND route_slug = $2)'
                INTO pending USING a.api_key_id, a.route_slug;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    SELECT * INTO e FROM olp.end_user_accounts WHERE id = owner;
    IF FOUND THEN
        IF e.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.api_key_id, e.end_user_digest;
        ELSE
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_usage WHERE api_key_project_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_project_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.project_id, e.end_user_digest;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    RETURN false;
END
$_$;

CREATE FUNCTION olp.has_pending_retained_tokens(owner uuid) RETURNS boolean
    LANGUAGE plpgsql STABLE
    AS $_$
DECLARE
    pending boolean;
    a olp.aggregate_budget_accounts;
    e olp.end_user_accounts;
    projects uuid[];
    column_name text;
BEGIN
    SELECT EXISTS(SELECT 1 FROM olp.api_keys k JOIN olp.provider_resource_pending_tokens p ON p.api_key_id=k.id WHERE k.budget_group_id=owner)
     OR EXISTS(SELECT 1 FROM olp.api_keys k JOIN olp.batch_pending_subjects p ON p.api_key_id=k.id WHERE k.budget_group_id=owner) INTO pending;
    IF pending THEN RETURN true; END IF;
    FOREACH column_name IN ARRAY ARRAY[
        'api_key_id', 'budget_group_id', 'provider_id', 'slot_id',
        'route_id', 'pinned_route_id', 'leg_route_id'
    ] LOOP
        EXECUTE format(
            'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE %1$I=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE %1$I=$1)',
            column_name
        ) INTO pending USING owner;
        IF pending THEN RETURN true; END IF;
    END LOOP;
    EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.pending_token_recorded_owners WHERE budget_owner_id=$1)
             OR EXISTS(SELECT 1 FROM olp.batch_pending_recorded_owners WHERE budget_owner_id=$1)'
        INTO pending USING owner;
    IF pending THEN RETURN true; END IF;
    SELECT * INTO a FROM olp.aggregate_budget_accounts WHERE id = owner;
    IF FOUND THEN
        IF a.installation_id IS NOT NULL THEN
            EXECUTE 'SELECT
                (SELECT resource_id FROM olp.provider_resource_pending_tokens ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL
                OR
                (SELECT resource_id FROM olp.batch_pending_subjects ORDER BY resource_id, ordinal LIMIT 1) IS NOT NULL'
                INTO pending;
        ELSIF a.organization_id IS NOT NULL THEN
            SELECT array_agg(id) INTO projects FROM olp.projects WHERE organization_id = a.organization_id;
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE project_id = ANY($1))
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = ANY($1))'
                INTO pending USING projects;
        ELSIF a.project_id IS NOT NULL AND a.attribution_key IS NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE project_id = $1)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1)'
                INTO pending USING a.project_id;
        ELSIF a.project_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE project_id = $1 AND attribution @> $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE project_id = $1 AND attribution @> $2)'
                INTO pending USING a.project_id, jsonb_build_object(a.attribution_key, a.attribution_value);
        ELSIF a.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE api_key_id = $1 AND route_slug = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND route_slug = $2)'
                INTO pending USING a.api_key_id, a.route_slug;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    SELECT * INTO e FROM olp.end_user_accounts WHERE id = owner;
    IF FOUND THEN
        IF e.api_key_id IS NOT NULL THEN
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE api_key_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.api_key_id, e.end_user_digest;
        ELSE
            EXECUTE 'SELECT EXISTS(SELECT 1 FROM olp.provider_resource_pending_tokens WHERE api_key_project_id = $1 AND end_user_digest = $2)
                     OR EXISTS(SELECT 1 FROM olp.batch_pending_subjects WHERE api_key_project_id = $1 AND end_user_digest = $2)'
                INTO pending USING e.project_id, e.end_user_digest;
        END IF;
        IF pending THEN RETURN true; END IF;
    END IF;
    RETURN false;
END
$_$;

CREATE FUNCTION olp.immutable_credential_reference() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF OLD.external_reference IS DISTINCT FROM NEW.external_reference THEN
  RAISE EXCEPTION 'credential reference versions are immutable';
 END IF;
 RETURN NEW;
END
$$;

CREATE FUNCTION olp.maintain_exports(p_limit integer) RETURNS bigint
    LANGUAGE plpgsql
    AS $$
DECLARE v_deleted bigint;
BEGIN
 WITH candidates AS MATERIALIZED (
  SELECT r.id,r.stream,r.occurred_at,
   r.occurred_at < now()-make_interval(days=>COALESCE((SELECT s.value::integer FROM olp.settings s WHERE s.key=
    CASE r.stream WHEN 'audit' THEN 'retention.audit_days' WHEN 'usage_rollups' THEN 'retention.usage_days' ELSE 'retention.requests_days' END),
    CASE r.stream WHEN 'audit' THEN 365 WHEN 'usage_rollups' THEN 90 ELSE 30 END)) AS expired
  FROM olp.export_records r
  WHERE NOT EXISTS(SELECT 1 FROM olp.export_pending p WHERE p.record_id=r.id AND p.delivered_at IS NULL AND p.rejected_at IS NULL)
   OR r.occurred_at < now()-make_interval(days=>COALESCE((SELECT s.value::integer FROM olp.settings s WHERE s.key=
    CASE r.stream WHEN 'audit' THEN 'retention.audit_days' WHEN 'usage_rollups' THEN 'retention.usage_days' ELSE 'retention.requests_days' END),
    CASE r.stream WHEN 'audit' THEN 365 WHEN 'usage_rollups' THEN 90 ELSE 30 END))
  ORDER BY r.occurred_at,r.id LIMIT p_limit FOR UPDATE OF r SKIP LOCKED
 ), locked_pending AS MATERIALIZED (
  SELECT p.sink_id,p.record_id,p.attempts,p.last_attempt_at,p.last_error_code,p.delivered_at,p.rejected_at,p.send_may_have_occurred
  FROM olp.export_pending p JOIN candidates c ON c.id=p.record_id FOR UPDATE OF p
 ), receipts AS (
  INSERT INTO olp.metering_delivery_receipts(sink_id,attempt_id,record_id,project_id,outcome,last_error_code,attempts,occurred_at,queued_at,last_attempt_at,settled_at)
  SELECT lp.sink_id,r.source_id,r.id,r.project_id,
   CASE WHEN lp.delivered_at IS NOT NULL THEN 'accepted'
        WHEN lp.send_may_have_occurred THEN 'delivery_unconfirmed'
        ELSE 'expired_before_delivery' END,
   CASE WHEN lp.delivered_at IS NOT NULL THEN NULL ELSE lp.last_error_code END,
   lp.attempts,r.occurred_at,r.queued_at,lp.last_attempt_at,COALESCE(lp.delivered_at,lp.rejected_at,now())
  FROM candidates c
  JOIN olp.export_records r ON r.id=c.id
  JOIN locked_pending lp ON lp.record_id=r.id
  JOIN olp.export_sinks s ON s.id=lp.sink_id
  WHERE r.stream='metering' AND s.format='cloudevents'
  ON CONFLICT(sink_id,attempt_id) DO UPDATE SET
   record_id=EXCLUDED.record_id,project_id=EXCLUDED.project_id,outcome=EXCLUDED.outcome,
   last_error_code=EXCLUDED.last_error_code,attempts=EXCLUDED.attempts,
   occurred_at=EXCLUDED.occurred_at,queued_at=EXCLUDED.queued_at,
   last_attempt_at=EXCLUDED.last_attempt_at,settled_at=EXCLUDED.settled_at
  WHERE metering_delivery_receipts.outcome<>'accepted'
  RETURNING sink_id
 ), gaps AS (
  INSERT INTO olp.export_gaps(sink_id,stream,record_count,first_occurred_at,last_occurred_at,reason)
  SELECT p.sink_id,c.stream,count(*),min(c.occurred_at),max(c.occurred_at),'retention_expired'
  FROM candidates c JOIN olp.export_pending p ON p.record_id=c.id
  WHERE c.expired AND p.delivered_at IS NULL AND p.rejected_at IS NULL GROUP BY p.sink_id,c.stream
  RETURNING sink_id,stream,record_count
 ), checkpointed_gaps AS (
  INSERT INTO olp.export_cursors(sink_id,stream,gaps_total)
  SELECT sink_id,stream,sum(record_count) FROM gaps GROUP BY sink_id,stream
  ON CONFLICT(sink_id,stream) DO UPDATE SET gaps_total=export_cursors.gaps_total+EXCLUDED.gaps_total,updated_at=now()
  RETURNING sink_id
 ), removed AS (
  DELETE FROM olp.export_records r USING candidates c WHERE r.id=c.id AND (SELECT count(*) FROM checkpointed_gaps)>=0 AND (SELECT count(*) FROM receipts)>=0 RETURNING r.id
 ) SELECT count(*) INTO v_deleted FROM removed;
 DELETE FROM olp.export_gaps WHERE recorded_at<now()-make_interval(days=>COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.audit_days'),365));
 DELETE FROM olp.metering_delivery_receipts WHERE settled_at<now()-make_interval(days=>COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.requests_days'),30));
 RETURN v_deleted;
END $$;

CREATE FUNCTION olp.materialize_commitments(p_now timestamp with time zone, p_cutoff timestamp with time zone, p_limit integer DEFAULT 512) RETURNS integer
    LANGUAGE plpgsql
    AS $$
DECLARE s olp.commitment_schedules; b timestamptz; n integer:=0;
BEGIN
 FOR s IN SELECT * FROM olp.commitment_schedules
  WHERE effective_at<=p_now AND COALESCE(materialized_through,effective_at)<=p_now
   AND (ends_at IS NULL OR COALESCE(materialized_through,effective_at)<ends_at)
  ORDER BY COALESCE(materialized_through,effective_at),id FOR UPDATE SKIP LOCKED
 LOOP
  b:=GREATEST(COALESCE(s.materialized_through,s.effective_at),olp.commitment_bucket(s.period,GREATEST(s.effective_at,p_cutoff)));
  b:=olp.commitment_bucket(s.period,b);
  WHILE b<=p_now AND (s.ends_at IS NULL OR b<s.ends_at) AND n<p_limit LOOP
   PERFORM olp.ensure_commitment_period(s.id,b);
   b:=olp.commitment_end(s.period,b); n:=n+1;
  END LOOP;
  UPDATE olp.commitment_schedules SET materialized_through=b WHERE id=s.id;
  EXIT WHEN n>=p_limit;
 END LOOP;
 RETURN n;
END $$;

CREATE FUNCTION olp.mcp_revision_immutable() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 RAISE EXCEPTION 'MCP server revisions are immutable' USING ERRCODE='55000';
END;
$$;

CREATE FUNCTION olp.metering_value(settings jsonb, payload jsonb) RETURNS numeric
    LANGUAGE sql IMMUTABLE
    AS $$
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

CREATE FUNCTION olp.observe_code_reference(target_attempt uuid, reference_id text) RETURNS void
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(a.route_id::text||':'||a.api_key_id::text,0))
    FROM olp.code_attempts a WHERE a.id=target_attempt;

    INSERT INTO olp.code_paid_references(route_id,api_key_id,external_id,binding_id)
    SELECT a.route_id,a.api_key_id,reference_id,a.paid_binding_id
    FROM olp.code_attempts a
    WHERE a.id=target_attempt AND a.paid_binding_id IS NOT NULL AND a.state<>'prepared'
    ON CONFLICT(route_id,api_key_id,external_id) DO UPDATE SET
        ambiguous=olp.code_paid_references.ambiguous OR
        (SELECT root_id FROM olp.code_paid_bindings WHERE id=olp.code_paid_references.binding_id)<>
        (SELECT root_id FROM olp.code_paid_bindings WHERE id=EXCLUDED.binding_id);

    INSERT INTO olp.code_references(route_id,api_key_id,external_id,binding_id,account_id)
    SELECT a.route_id,a.api_key_id,reference_id,a.binding_id,a.account_id
    FROM olp.code_attempts a
    WHERE a.id=target_attempt AND a.state<>'prepared' AND a.paid_binding_id IS NULL
    ON CONFLICT(route_id,api_key_id,external_id) DO UPDATE SET
        ambiguous=olp.code_references.ambiguous OR
        olp.code_references.account_id IS DISTINCT FROM EXCLUDED.account_id OR
        (SELECT root_id FROM olp.code_bindings WHERE id=olp.code_references.binding_id)<>
        (SELECT root_id FROM olp.code_bindings WHERE id=EXCLUDED.binding_id);

    UPDATE olp.code_paid_references p SET ambiguous=true
    FROM olp.code_attempts a
    WHERE a.id=target_attempt AND p.route_id=a.route_id AND p.api_key_id=a.api_key_id
        AND p.external_id=reference_id AND EXISTS(
            SELECT 1 FROM olp.code_references r WHERE r.route_id=p.route_id
                AND r.api_key_id=p.api_key_id AND r.external_id=p.external_id);

    UPDATE olp.code_references r SET ambiguous=true
    FROM olp.code_attempts a
    WHERE a.id=target_attempt AND r.route_id=a.route_id AND r.api_key_id=a.api_key_id
        AND r.external_id=reference_id AND EXISTS(
            SELECT 1 FROM olp.code_paid_references p WHERE p.route_id=r.route_id
                AND p.api_key_id=r.api_key_id AND p.external_id=r.external_id);
END
$$;

CREATE TABLE olp.provider_resource_pending_usage (
    resource_id uuid NOT NULL,
    ordinal integer NOT NULL,
    api_key_id uuid NOT NULL,
    budget_group_id uuid,
    project_id uuid,
    api_key_project_id uuid,
    route_id uuid,
    leg_route_id uuid,
    pinned_route_id uuid,
    budget_owner_ids uuid[] NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL,
    slot_id uuid,
    attribution jsonb NOT NULL,
    end_user_digest text,
    CONSTRAINT provider_resource_pending_usage_ordinal_check CHECK ((ordinal > 0))
);

CREATE TABLE olp.provider_resources (
    id uuid NOT NULL,
    kind text NOT NULL,
    api_key_id uuid NOT NULL,
    route_slug text NOT NULL,
    provider_id uuid NOT NULL,
    provider_revision_id uuid NOT NULL,
    route_revision_id uuid NOT NULL,
    slot_id uuid NOT NULL,
    credential_id uuid,
    upstream_id text NOT NULL,
    state text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    expires_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    contract_version text,
    parent_id uuid,
    submission_id text,
    target_id uuid,
    mutation_version bigint DEFAULT 0 NOT NULL,
    CONSTRAINT provider_resources_continuation_contract_check CHECK (((kind <> ALL (ARRAY['continuation'::text, 'strict_response'::text])) OR ((contract_version IS NOT NULL) AND (expires_at IS NOT NULL)))),
    CONSTRAINT provider_resources_contract_version_check CHECK (((contract_version IS NULL) OR ((octet_length(contract_version) >= 1) AND (octet_length(contract_version) <= 128)))),
    CONSTRAINT provider_resources_interaction_contract_check CHECK (((kind <> 'interaction'::text) OR ((contract_version IS NOT NULL) AND (contract_version = 'gemini-interaction/v1beta'::text) AND (expires_at IS NOT NULL) AND (expires_at <= (created_at + '24:00:00'::interval)) AND (upstream_id = (id)::text) AND (submission_id IS NULL)))),
    CONSTRAINT provider_resources_kind_check CHECK ((kind = ANY (ARRAY['file'::text, 'batch'::text, 'response'::text, 'continuation'::text, 'strict_response'::text, 'interaction'::text, 'strict_file'::text, 'strict_batch'::text, 'anthropic_file'::text, 'anthropic_batch'::text, 'gemini_upload'::text, 'gemini_file'::text, 'gemini_batch'::text, 'cloud_batch'::text, 'fine_tuning_job'::text, 'fine_tuning_event'::text, 'fine_tuning_checkpoint'::text, 'vector_store'::text, 'vector_store_file'::text, 'vector_store_batch'::text, 'conversation'::text, 'conversation_item'::text, 'container'::text, 'container_file'::text, 'eval'::text, 'eval_run'::text, 'eval_output_item'::text, 'skill'::text, 'skill_version'::text, 'context_cache'::text, 'agent_resource'::text, 'vertex_tuning_job'::text]))),
    CONSTRAINT provider_resources_metadata_check CHECK ((octet_length((metadata)::text) <= 16384)),
    CONSTRAINT provider_resources_mutation_version_check CHECK ((mutation_version >= 0)),
    CONSTRAINT provider_resources_strict_durable_check CHECK (((kind <> ALL (ARRAY['strict_file'::text, 'strict_batch'::text])) OR ((contract_version = 'native-durable-v1'::text) AND (expires_at IS NOT NULL) AND (expires_at <= (created_at + '7 days'::interval)) AND (submission_id IS NULL)))),
    CONSTRAINT provider_resources_submission_check CHECK (((submission_id IS NULL) OR ((kind = 'continuation'::text) AND ((octet_length(submission_id) >= 1) AND (octet_length(submission_id) <= 128))))),
    CONSTRAINT provider_resources_upstream_id_check CHECK (((octet_length(upstream_id) >= 1) AND (octet_length(upstream_id) <= 512)))
);

COMMENT ON COLUMN olp.provider_resources.contract_version IS 'Version of the resource-owned encrypted interaction contract. Kinds that store an encrypted payload under a contract version, such as strict responses, files and batches, stay separate from the metadata-only kinds of transformed routes.';

COMMENT ON COLUMN olp.provider_resources.parent_id IS 'Immutable logical lineage, checked by the resource owner on creation. It may outlive an expired parent; children contain complete bounded state and do not retain parent payloads by foreign key.';

COMMENT ON COLUMN olp.provider_resources.submission_id IS 'Owner-scoped client submission identity for accepted-work and delivery replay. Never an authorization credential.';

CREATE FUNCTION olp.pending_resource_subjects(pr olp.provider_resources) RETURNS SETOF olp.provider_resource_pending_usage
    LANGUAGE sql STABLE
    AS $$
SELECT pr.id,(a->>'ordinal')::integer,pr.api_key_id,
           NULLIF(pr.metadata->'pending_usage'->>'budget_group_id','')::uuid,
           COALESCE(k.project_id,p.project_id,r.project_id),k.project_id,
           r.id,leg_r.id,pinned_r.id,ARRAY(SELECT b::uuid FROM jsonb_array_elements_text(COALESCE(a->'routing'->'budgets','[]'::jsonb)) b),pr.metadata->'pending_usage'->>'route_slug',(a->>'provider_id')::uuid,
           NULLIF(a->'routing'->>'credential_slot_id','')::uuid,
           COALESCE(pr.metadata->'pending_usage'->'attribution','{}'::jsonb),
           NULLIF(pr.metadata->'pending_usage'->>'end_user_digest','')
    FROM jsonb_array_elements(CASE WHEN olp.resource_owns_pending_usage(pr.kind)
         THEN pr.metadata->'pending_usage'->'attempts' ELSE '[]'::jsonb END) a
    JOIN olp.api_keys k ON k.id=pr.api_key_id
    JOIN olp.providers p ON p.id=(a->>'provider_id')::uuid
    LEFT JOIN olp.routes r ON r.slug=pr.metadata->'pending_usage'->>'route_slug'
    LEFT JOIN olp.routes pinned_r ON pinned_r.slug=pr.route_slug
    LEFT JOIN olp.routes leg_r ON leg_r.slug=a->'routing'->'leg'->>'route'
    WHERE NOT COALESCE((a->'routing'->>'budget_exempt')::boolean,false);
$$;

CREATE FUNCTION olp.preserve_code_account() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
    IF NEW.project_id <> OLD.project_id OR NEW.provider_id <> OLD.provider_id OR NEW.principal <> OLD.principal THEN
        RAISE EXCEPTION 'code account identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION olp.preserve_code_binding() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
    IF TG_OP='UPDATE' AND olp.code_initial_recovery_safe(OLD.id)
       AND to_jsonb(NEW)-ARRAY['account_id','principal'] IS NOT DISTINCT FROM to_jsonb(OLD)-ARRAY['account_id','principal'] THEN
        RETURN NEW;
    END IF;
    IF TG_OP='DELETE' THEN
        RAISE EXCEPTION 'code binding retirement is durable' USING ERRCODE='23514';
    END IF;
    IF to_jsonb(NEW)-ARRAY['retired_at','socket_bound'] IS DISTINCT FROM to_jsonb(OLD)-ARRAY['retired_at','socket_bound']
       OR OLD.socket_bound AND NOT NEW.socket_bound
       OR OLD.retired_at IS DISTINCT FROM NEW.retired_at AND (OLD.retired_at IS NOT NULL OR NEW.retired_at IS NULL) THEN
        RAISE EXCEPTION 'code binding changes only by retirement, socket ownership or proven portable nonexecution' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION olp.preserve_code_pin() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
    IF TG_OP='UPDATE' AND olp.code_initial_recovery_safe(OLD.root_id)
       AND to_jsonb(NEW)-ARRAY['account_id','principal','adapter','target_model'] IS NOT DISTINCT FROM to_jsonb(OLD)-ARRAY['account_id','principal','adapter','target_model']
       AND (NEW.target_model IS NOT DISTINCT FROM OLD.target_model OR NEW.target_model=coalesce(OLD.target_model,OLD.model) OR EXISTS(
           SELECT 1 FROM olp.code_bindings b JOIN olp.code_routes r ON r.id=b.route_id JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id
           WHERE b.id=OLD.root_id AND v.document->>'model_rewriting'='true'
             AND v.document->'model_pools'->OLD.model ? NEW.target_model AND v.document->'models' ? NEW.target_model)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'code pins are durable after execution' USING ERRCODE='23514';
END $$;

CREATE FUNCTION olp.preserve_provider_credential() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
    IF OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL
       OR to_jsonb(NEW) - 'revoked_at' IS DISTINCT FROM to_jsonb(OLD) - 'revoked_at' THEN
        RAISE EXCEPTION 'a credential version changes only by revocation'
            USING ERRCODE = '23514', CONSTRAINT = 'provider_credential_immutable';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION olp.preserve_route_slug() RETURNS trigger
    LANGUAGE plpgsql
    AS $$ BEGIN
    IF NEW.slug IS DISTINCT FROM OLD.slug THEN
        RAISE EXCEPTION 'route slug is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'route_slug_identity';
    END IF;
    RETURN NEW;
END $$;

CREATE FUNCTION olp.price_first_rate(rates jsonb, VARIADIC names text[]) RETURNS TABLE(rate numeric, field text)
    LANGUAGE sql IMMUTABLE
    AS $$
    SELECT (rates->>name)::numeric(24,12), name FROM unnest(names) AS name
    WHERE rates->>name IS NOT NULL LIMIT 1
$$;

CREATE FUNCTION olp.price_usage(price jsonb, usage jsonb, started_at timestamp with time zone, successful boolean, discount numeric DEFAULT 1) RETURNS TABLE(complete boolean, list_cost numeric, cost numeric, components text[])
    LANGUAGE plpgsql STABLE
    AS $$
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
    v_invocations bigint := COALESCE((usage->>'invocation_count')::bigint,CASE WHEN COALESCE((usage->>'deferred')::boolean,false) THEN 0 ELSE 1 END);
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
    -- Aggregate eval totals cannot choose each invocation's input tier.
    IF COALESCE((usage->>'deferred')::boolean,false) AND v_invocations>1
       AND jsonb_typeof(price->'tiers')='array' THEN
        IF jsonb_array_length(price->'tiers')>0 THEN
            RETURN QUERY SELECT false, NULL::numeric, NULL::numeric, NULL::text[];
            RETURN;
        END IF;
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

    IF successful AND v_invocations>0 AND price->>'per_request' IS NOT NULL THEN
        v_charge := v_charge + v_invocations*(price->>'per_request')::numeric;
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

    -- Exact retained units. Omitted quantities remain unknown, never zero.
    FOR share IN SELECT * FROM (VALUES
      ('training_tokens','training_per_million',1000000::numeric,'1000000 Training Tokens'),
      ('storage_byte_seconds','storage_per_gib_day',1073741824::numeric*86400,'GiB Day'),
      ('cache_token_seconds','cache_storage_per_million_token_hour',1000000::numeric*3600,'1000000 Token Hours')
    ) AS dimensions(quantity,rate,divisor,unit) LOOP
      IF usage->>share.quantity IS NOT NULL THEN
        fee:=(price->>share.rate)::numeric;
        IF fee IS NULL THEN v_ok:=false;
        ELSE v_charge:=v_charge+((usage->>share.quantity)::numeric*fee)::numeric(600,300)/share.divisor;
          v_comp:=v_comp||share.rate;
        END IF;
      END IF;
    END LOOP;
    FOR tool IN SELECT key,value::bigint AS calls FROM jsonb_each_text(COALESCE(usage->'container_sessions','{}'::jsonb)) LOOP
      fee:=(price->'container_per_session'->>tool.key)::numeric;
      IF fee IS NULL THEN v_ok:=false;
      ELSE v_charge:=v_charge+tool.calls*fee; v_comp:=v_comp||('container_per_session.'||tool.key);
      END IF;
    END LOOP;

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

CREATE FUNCTION olp.price_usage_lines(price jsonb, usage jsonb, started_at timestamp with time zone, successful boolean, discount numeric DEFAULT 1) RETURNS jsonb
    LANGUAGE plpgsql STABLE
    AS $$
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
    v_invocations bigint := COALESCE((usage->>'invocation_count')::bigint,CASE WHEN COALESCE((usage->>'deferred')::boolean,false) THEN 0 ELSE 1 END);
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
    -- Aggregate eval totals cannot choose each invocation's input tier.
    IF COALESCE((usage->>'deferred')::boolean,false) AND v_invocations>1
       AND jsonb_typeof(price->'tiers')='array' THEN
        IF jsonb_array_length(price->'tiers')>0 THEN
            RETURN NULL;
        END IF;
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

    IF successful AND v_invocations>0 AND price->>'per_request' IS NOT NULL THEN
        v_base := v_base || jsonb_build_object('component', 'per_request', 'tier', NULL,
            'unit', 'Requests', 'quantity', v_invocations::text,
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

    -- Exact retained units. Omitted quantities remain unknown, never zero.
    FOR share IN SELECT * FROM (VALUES
      ('training_tokens','training_per_million',1000000::numeric,'1000000 Training Tokens'),
      ('storage_byte_seconds','storage_per_gib_day',1073741824::numeric*86400,'GiB Day'),
      ('cache_token_seconds','cache_storage_per_million_token_hour',1000000::numeric*3600,'1000000 Token Hours')
    ) AS dimensions(quantity,rate,divisor,unit) LOOP
      IF usage->>share.quantity IS NOT NULL THEN
        fee:=(price->>share.rate)::numeric;
        IF fee IS NULL THEN v_ok:=false;
        ELSIF (usage->>share.quantity)::numeric>0 THEN
          v_base:=v_base||jsonb_build_object('component',share.rate,'tier',NULL,'unit',share.unit,
            'quantity',((usage->>share.quantity)::numeric(600,300)/share.divisor)::text,'base',fee::text);
        END IF;
      END IF;
    END LOOP;
    FOR tool IN SELECT key,value::bigint AS calls FROM jsonb_each_text(COALESCE(usage->'container_sessions','{}'::jsonb)) LOOP
      fee:=(price->'container_per_session'->>tool.key)::numeric;
      IF fee IS NULL THEN v_ok:=false;
      ELSIF tool.calls>0 THEN v_base:=v_base||jsonb_build_object('component','container_per_session.'||tool.key,'tier',tool.key,
        'unit','Session','quantity',tool.calls::text,'base',fee::text);
      END IF;
    END LOOP;

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

CREATE FUNCTION olp.project_batch_pending_recorded_owners() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO olp.batch_pending_recorded_owners(resource_id, ordinal, budget_owner_id)
    SELECT NEW.resource_id, NEW.ordinal, owner FROM unnest(NEW.budget_owner_ids) owner
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END
$$;

CREATE FUNCTION olp.project_batch_settlement_subjects() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE
    was_open boolean;
    is_open boolean;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        was_open := OLD.state IN ('submitting', 'submission_unknown', 'active', 'settling', 'conflict');
        is_open := NEW.state IN ('submitting', 'submission_unknown', 'active', 'settling', 'conflict');
        IF was_open IS NOT DISTINCT FROM is_open
           AND NEW.template IS NOT DISTINCT FROM OLD.template THEN
            RETURN NEW;
        END IF;
    END IF;
    DELETE FROM olp.batch_pending_subjects WHERE resource_id = NEW.resource_id;
    IF NEW.state IN ('submitting', 'submission_unknown', 'active', 'settling', 'conflict') THEN
        INSERT INTO olp.batch_pending_subjects
        SELECT * FROM olp.batch_settlement_subjects(NEW);
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION olp.project_pending_recorded_owners() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO olp.pending_resource_recorded_owners(resource_id,ordinal,budget_owner_id)
    SELECT NEW.resource_id,NEW.ordinal,owner FROM unnest(NEW.budget_owner_ids) owner
    ON CONFLICT DO NOTHING;
    RETURN NEW;
END $$;

CREATE FUNCTION olp.project_pending_resource_usage() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM olp.retained_settlements WHERE resource_id=NEW.id) THEN RETURN NEW; END IF;
 IF NOT olp.resource_owns_pending_usage(NEW.kind) THEN
  DELETE FROM olp.provider_resource_pending_usage WHERE resource_id=NEW.id; RETURN NEW;
 END IF;
 IF TG_OP='UPDATE' AND NEW.kind IS NOT DISTINCT FROM OLD.kind AND NEW.metadata->'pending_usage' IS NOT DISTINCT FROM OLD.metadata->'pending_usage' THEN RETURN NEW; END IF;
 DELETE FROM olp.provider_resource_pending_usage WHERE resource_id=NEW.id;
 IF NEW.metadata->'pending_usage' IS NOT NULL AND NEW.metadata->'pending_usage'<>'null'::jsonb THEN
  INSERT INTO olp.provider_resource_pending_usage SELECT * FROM olp.pending_resource_subjects(NEW);
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.project_retained_liability() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
DECLARE pr olp.provider_resources; template jsonb; v_ordinal integer:=0;
BEGIN
 DELETE FROM olp.provider_resource_pending_usage WHERE resource_id=NEW.resource_id;
 IF NEW.state='closed' THEN RETURN NEW; END IF;
 SELECT * INTO pr FROM olp.provider_resources WHERE id=NEW.resource_id;
 FOR template IN SELECT value FROM jsonb_each(NEW.templates) LOOP
  v_ordinal:=v_ordinal+1;
  pr.kind:='response';
  pr.metadata:=jsonb_build_object('pending_usage',template);
  INSERT INTO olp.provider_resource_pending_usage
  SELECT resource_id,v_ordinal,api_key_id,budget_group_id,project_id,api_key_project_id,route_id,leg_route_id,pinned_route_id,budget_owner_ids,route_slug,provider_id,slot_id,attribution,end_user_digest
   FROM olp.pending_resource_subjects(pr);
 END LOOP;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.publish_commitments(p_at timestamp with time zone) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE d record; s olp.commitment_schedules; boundary timestamptz; cadence text;
BEGIN
 FOR d IN
  SELECT p.id, r.id revision_id, r.name, p.kind, r.configuration::jsonb cfg,
   p.project_id, pj.name project_name,
   r.configuration::jsonb#>'{options,commitment}' desired
  FROM olp.providers p JOIN olp.provider_revisions r ON r.id=p.active_revision_id
  LEFT JOIN olp.projects pj ON pj.id=p.project_id WHERE p.state IN ('active','disabled')
  UNION ALL
  SELECT DISTINCT cs.provider_id,NULL::uuid,NULL,NULL,NULL::jsonb,NULL::uuid,NULL,NULL::jsonb
  FROM olp.commitment_schedules cs WHERE NOT EXISTS (
   SELECT 1 FROM olp.providers p WHERE p.id=cs.provider_id AND p.active_revision_id IS NOT NULL AND p.state IN ('active','disabled'))
 LOOP
  SELECT * INTO s FROM olp.commitment_schedules WHERE provider_id=d.id AND effective_at<=p_at
   AND (ends_at IS NULL OR ends_at>p_at) ORDER BY effective_at DESC LIMIT 1 FOR UPDATE;
  IF FOUND AND d.desired->>'amount' IS NOT NULL
   AND s.amount=(d.desired->>'amount')::numeric AND s.period=d.desired->>'period'
   AND s.provider_name=d.name AND s.project_id IS NOT DISTINCT FROM d.project_id
   AND s.vendor_id IS NOT DISTINCT FROM d.cfg#>>'{options,vendor_id}' THEN
   DELETE FROM olp.commitment_schedules WHERE provider_id=d.id AND effective_at>p_at;
   UPDATE olp.commitment_schedules SET ends_at=NULL WHERE id=s.id;
   CONTINUE;
  END IF;
  DELETE FROM olp.commitment_schedules WHERE provider_id=d.id AND effective_at>p_at;
  boundary:=p_at;
  IF s.id IS NOT NULL THEN
   cadence:=s.period;
   IF d.desired->>'period'='month' OR s.period='month' THEN cadence:='month';
   ELSIF d.desired->>'period'='day' OR s.period='day' THEN cadence:='day'; END IF;
   boundary:=olp.commitment_end(cadence,olp.commitment_bucket(cadence,p_at));
   UPDATE olp.commitment_schedules SET ends_at=boundary WHERE id=s.id;
  END IF;
  IF d.desired->>'amount' IS NOT NULL THEN
   INSERT INTO olp.commitment_schedules(provider_id,provider_revision_id,provider_name,provider_kind,vendor_id,
    project_id,project_name,amount,currency,period,effective_at)
   VALUES(d.id,d.revision_id,d.name,d.kind,d.cfg#>>'{options,vendor_id}',d.project_id,d.project_name,
    (d.desired->>'amount')::numeric,COALESCE((SELECT btrim(currency) FROM olp.pricing_currency WHERE singleton),'USD'),
    d.desired->>'period',boundary);
  END IF;
 END LOOP;
END $$;

CREATE FUNCTION olp.record_shared_quota_retirements() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    INSERT INTO olp.runtime_shared_quota_retirements (sequence, project_id, group_name)
    SELECT NEW.sequence, retired.project_id, retired.group_name
    FROM (
        SELECT * FROM olp.snapshot_shared_quota_groups((
            SELECT snapshot FROM olp.runtime_releases
            WHERE sequence < NEW.sequence ORDER BY sequence DESC LIMIT 1
        ))
        EXCEPT
        SELECT * FROM olp.snapshot_shared_quota_groups(NEW.snapshot)
    ) retired;
    RETURN NEW;
END
$$;

CREATE FUNCTION olp.reject_execution_evidence_mutation() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN RAISE EXCEPTION 'execution evidence is immutable'; END $$;

CREATE FUNCTION olp.resource_owns_pending_usage(kind text) RETURNS boolean
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
    SELECT kind IN ('response','strict_response','interaction');
$$;

CREATE FUNCTION olp.scale_price_lines(lines jsonb, factor numeric) RETURNS jsonb
    LANGUAGE plpgsql IMMUTABLE
    AS $$
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

CREATE FUNCTION olp.schedule_budget_zone(zone_name text, instant timestamp with time zone) RETURNS void
    LANGUAGE plpgsql
    AS $$
DECLARE kind text; current_window record;
BEGIN
 -- Validate against the database's installed IANA rules as well as Go's rules.
 PERFORM instant AT TIME ZONE zone_name;
 FOREACH kind IN ARRAY ARRAY['day','week','month'] LOOP
  DELETE FROM olp.budget_calendar_changes WHERE window_kind=kind AND effective_at>instant;
  SELECT * INTO STRICT current_window FROM olp.budget_window(kind,instant);
  IF current_window.time_zone<>zone_name THEN
   INSERT INTO olp.budget_calendar_changes VALUES(kind,current_window.end_at,zone_name);
  END IF;
 END LOOP;
END $$;

CREATE FUNCTION olp.scim_touch_member_groups() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 UPDATE olp.scim_groups SET etag=uuidv7(),updated_at=now()
 WHERE id IN (SELECT group_id FROM olp.scim_group_members WHERE user_id=NEW.id);
 RETURN NULL;
END $$;

CREATE FUNCTION olp.scim_touch_removed_member_groups() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 UPDATE olp.scim_groups SET etag=uuidv7(),updated_at=now()
 WHERE id IN (SELECT group_id FROM removed_scim_members);
 RETURN NULL;
END $$;

CREATE FUNCTION olp.snapshot_budget_notification_subject() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
 IF NEW.window_id IS NOT NULL AND num_nonnulls(NEW.budget_subject_kind,NEW.budget_subject_id,NEW.budget_window_kind)=0 THEN
  SELECT subject_kind,subject_id,window_kind INTO NEW.budget_subject_kind,NEW.budget_subject_id,NEW.budget_window_kind
  FROM olp.notification_rules WHERE id=NEW.rule_id FOR SHARE;
 END IF;
 RETURN NEW;
END $$;

CREATE FUNCTION olp.snapshot_shared_quota_groups(snapshot json) RETURNS TABLE(project_id text, group_name text)
    LANGUAGE sql IMMUTABLE STRICT
    AS $$
    SELECT DISTINCT provider.value->>'project_id', quota.value->>'group'
    FROM json_each(CASE WHEN json_typeof(snapshot->'providers') = 'object'
                        THEN snapshot->'providers' ELSE '{}'::json END) provider
    CROSS JOIN LATERAL json_array_elements(
        CASE WHEN json_typeof(provider.value->'shared_token_quotas') = 'array'
             THEN provider.value->'shared_token_quotas' ELSE '[]'::json END
    ) quota
    WHERE provider.value->>'project_id' IS NOT NULL AND quota.value->>'group' IS NOT NULL
$$;

CREATE TABLE olp.a2a_task_admissions (
    dispatch_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    task_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    callback_attempts integer DEFAULT 0 NOT NULL,
    CONSTRAINT a2a_task_admissions_callback_attempts_check CHECK (((callback_attempts >= 0) AND (callback_attempts <= 128)))
);

CREATE TABLE olp.a2a_task_callback_accounting (
    dispatch_id uuid NOT NULL,
    kind text NOT NULL,
    accounting_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT a2a_task_callback_accounting_kind_check CHECK ((kind = ANY (ARRAY['inference'::text, 'tools'::text])))
);

CREATE TABLE olp.a2a_task_capabilities (
    dispatch_id uuid NOT NULL,
    token_digest bytea NOT NULL,
    publication_id uuid NOT NULL,
    revision_id uuid NOT NULL,
    task_id uuid,
    expires_at timestamp with time zone NOT NULL,
    caller_workload_revision_id uuid,
    caller_expires_at timestamp with time zone,
    end_user_digest text DEFAULT ''::text NOT NULL,
    delegation_token_digest bytea,
    CONSTRAINT a2a_task_capabilities_token_digest_check CHECK ((octet_length(token_digest) = 32))
);

CREATE TABLE olp.a2a_tasks (
    id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    project_id uuid NOT NULL,
    publication_id uuid NOT NULL,
    revision_id uuid NOT NULL,
    dispatch_id uuid NOT NULL,
    upstream_id text NOT NULL,
    upstream_context_id text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    caller_workload_revision_id uuid,
    caller_expires_at timestamp with time zone,
    state text DEFAULT 'outcome_unknown'::text NOT NULL,
    deadline_at timestamp with time zone DEFAULT (now() + '00:10:00'::interval) NOT NULL,
    continuations integer DEFAULT 0 NOT NULL,
    cancel_claimed_until timestamp with time zone,
    oauth_binding_digest text,
    delegation_pair_digest text DEFAULT ''::text NOT NULL,
    delegation_agent_id uuid,
    delegation_member_id uuid,
    delegation_token_digest text DEFAULT ''::text NOT NULL,
    callback_attempts integer DEFAULT 0,
    CONSTRAINT a2a_tasks_callback_attempts_check CHECK (((callback_attempts >= 0) AND (callback_attempts <= 128))),
    CONSTRAINT a2a_tasks_continuations_check CHECK (((continuations >= 0) AND (continuations <= 128))),
    CONSTRAINT a2a_tasks_oauth_binding_digest_check CHECK (((oauth_binding_digest IS NULL) OR (oauth_binding_digest ~ '^[0-9a-f]{64}$'::text))),
    CONSTRAINT a2a_tasks_state_check CHECK ((state = ANY (ARRAY['active'::text, 'completed'::text, 'failed'::text, 'canceled'::text, 'outcome_unknown'::text]))),
    CONSTRAINT a2a_tasks_upstream_context_id_check CHECK ((length(upstream_context_id) <= 1024)),
    CONSTRAINT a2a_tasks_upstream_id_check CHECK (((length(upstream_id) >= 1) AND (length(upstream_id) <= 1024)))
);

CREATE TABLE olp.agent_approvals (
    id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    project_id uuid NOT NULL,
    run_id uuid NOT NULL,
    binding_digest bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    dispatch_id uuid,
    CONSTRAINT agent_approvals_binding_digest_check CHECK ((octet_length(binding_digest) = 32)),
    CONSTRAINT agent_approvals_check CHECK (((consumed_at IS NULL) = (dispatch_id IS NULL)))
);

CREATE TABLE olp.agent_credentials (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    entra_client boolean DEFAULT false NOT NULL
);

CREATE TABLE olp.agent_delegations (
    token_digest bytea NOT NULL,
    project_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    issuer_revision uuid NOT NULL,
    allowed_routes text[] NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    consumed_at timestamp with time zone,
    revoked_at timestamp with time zone,
    consent_digest bytea,
    consenting_user_id uuid,
    scopes text[] DEFAULT ARRAY['inference'::text] NOT NULL,
    allowed_toolsets text[] DEFAULT ARRAY[]::text[] NOT NULL,
    allowed_agents text[] DEFAULT ARRAY[]::text[] NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    CONSTRAINT agent_delegations_resources_check CHECK ((((cardinality(allowed_routes) + cardinality(allowed_toolsets)) + cardinality(allowed_agents)) > 0))
);

CREATE TABLE olp.agent_publication_revisions (
    id uuid NOT NULL,
    publication_id uuid NOT NULL,
    project_id uuid NOT NULL,
    kind text NOT NULL,
    definition jsonb NOT NULL,
    digest text NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT agent_publication_revisions_definition_check CHECK (((jsonb_typeof(definition) = 'object'::text) AND (octet_length((definition)::text) <= 1048576))),
    CONSTRAINT agent_publication_revisions_digest_check CHECK ((digest ~ '^[a-f0-9]{64}$'::text)),
    CONSTRAINT agent_publication_revisions_kind_check CHECK ((kind = ANY (ARRAY['toolset'::text, 'agent'::text, 'prompt'::text])))
);

CREATE TABLE olp.agent_publications (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    kind text NOT NULL,
    name text NOT NULL,
    draft jsonb NOT NULL,
    latest_revision_id uuid,
    etag uuid NOT NULL,
    retired boolean DEFAULT false NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT agent_publications_draft_check CHECK (((jsonb_typeof(draft) = 'object'::text) AND (octet_length((draft)::text) <= 1048576))),
    CONSTRAINT agent_publications_kind_check CHECK ((kind = ANY (ARRAY['toolset'::text, 'agent'::text, 'prompt'::text]))),
    CONSTRAINT agent_publications_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100)))
);

CREATE TABLE olp.agent_runs (
    id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    project_id uuid NOT NULL,
    revision_id uuid,
    principal_digest bytea NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    route_revision_id uuid,
    CONSTRAINT agent_runs_principal_digest_check CHECK ((octet_length(principal_digest) = 32)),
    CONSTRAINT agent_runs_revision_kind CHECK (((revision_id IS NULL) <> (route_revision_id IS NULL)))
);

CREATE TABLE olp.agent_steps (
    id uuid NOT NULL,
    run_id uuid NOT NULL,
    dispatch_id uuid NOT NULL,
    ordinal integer NOT NULL,
    CONSTRAINT agent_steps_ordinal_check CHECK ((ordinal >= 0))
);

CREATE TABLE olp.aggregate_budget_accounts (
    id uuid NOT NULL,
    installation_id uuid,
    project_id uuid,
    initialized boolean DEFAULT false NOT NULL,
    api_key_id uuid,
    route_slug text,
    attribution_key text,
    attribution_value text,
    organization_id uuid,
    CONSTRAINT aggregate_attribution_owner CHECK ((((attribution_key IS NULL) = (attribution_value IS NULL)) AND ((attribution_key IS NULL) OR (project_id IS NOT NULL)))),
    CONSTRAINT aggregate_budget_accounts_attribution_key_check CHECK ((attribution_key ~ '^[A-Za-z][A-Za-z0-9_.-]{0,31}$'::text)),
    CONSTRAINT aggregate_budget_accounts_attribution_value_check CHECK ((attribution_value ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'::text)),
    CONSTRAINT aggregate_budget_accounts_route_slug_check CHECK ((route_slug ~ '^[a-z0-9][a-z0-9._-]{0,99}$'::text)),
    CONSTRAINT aggregate_budget_owner CHECK (((num_nonnulls(installation_id, organization_id, project_id, api_key_id) = 1) AND ((api_key_id IS NULL) = (route_slug IS NULL))))
);

CREATE TABLE olp.aggregate_cost_windows (
    account_id uuid NOT NULL,
    window_kind text NOT NULL,
    window_id bigint NOT NULL,
    accrued numeric(28,12) DEFAULT 0 NOT NULL,
    unpriced_attempts bigint DEFAULT 0 NOT NULL,
    weekly_complete boolean DEFAULT false NOT NULL,
    charged numeric(28,12) DEFAULT 0 NOT NULL,
    uncharged_attempts bigint DEFAULT 0 NOT NULL,
    CONSTRAINT aggregate_cost_windows_accrued_check CHECK ((accrued >= (0)::numeric)),
    CONSTRAINT aggregate_cost_windows_charged_check CHECK ((charged >= (0)::numeric)),
    CONSTRAINT aggregate_cost_windows_check CHECK (((window_kind = 'month'::text) OR (unpriced_attempts = 0))),
    CONSTRAINT aggregate_cost_windows_uncharged_attempts_check CHECK ((uncharged_attempts >= 0)),
    CONSTRAINT aggregate_cost_windows_uncharged_scope_check CHECK (((window_kind = 'month'::text) OR (uncharged_attempts = 0))),
    CONSTRAINT aggregate_cost_windows_unpriced_attempts_check CHECK ((unpriced_attempts >= 0)),
    CONSTRAINT aggregate_cost_windows_window_id_check CHECK ((window_id >= 0)),
    CONSTRAINT aggregate_cost_windows_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text])))
);

CREATE TABLE olp.api_key_cost_windows (
    api_key_id uuid NOT NULL,
    window_kind text NOT NULL,
    window_id bigint NOT NULL,
    accrued numeric(28,12) NOT NULL,
    unpriced_attempts bigint NOT NULL,
    weekly_complete boolean DEFAULT false NOT NULL,
    charged numeric(28,12) DEFAULT 0 NOT NULL,
    uncharged_attempts bigint DEFAULT 0 NOT NULL,
    CONSTRAINT api_key_cost_windows_accrued_check CHECK ((accrued >= (0)::numeric)),
    CONSTRAINT api_key_cost_windows_charged_check CHECK ((charged >= (0)::numeric)),
    CONSTRAINT api_key_cost_windows_uncharged_attempts_check CHECK ((uncharged_attempts >= 0)),
    CONSTRAINT api_key_cost_windows_uncharged_scope_check CHECK (((window_kind = 'month'::text) OR (uncharged_attempts = 0))),
    CONSTRAINT api_key_cost_windows_unpriced_attempts_check CHECK ((unpriced_attempts >= 0)),
    CONSTRAINT api_key_cost_windows_unpriced_scope_check CHECK (((window_kind = 'month'::text) OR (unpriced_attempts = 0))),
    CONSTRAINT api_key_cost_windows_window_id_check CHECK ((window_id >= 0)),
    CONSTRAINT api_key_cost_windows_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text])))
);

CREATE TABLE olp.api_key_overlaps (
    lookup_id text NOT NULL,
    api_key_id uuid NOT NULL,
    digest bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    last_observed_use timestamp with time zone,
    last_recording_error_at timestamp with time zone,
    CONSTRAINT api_key_overlaps_digest_check CHECK ((octet_length(digest) = 32))
);

CREATE TABLE olp.api_keys (
    id uuid NOT NULL,
    lookup_id text NOT NULL,
    digest bytea NOT NULL,
    name text NOT NULL,
    created_by uuid NOT NULL,
    policy jsonb NOT NULL,
    etag uuid NOT NULL,
    expires_at timestamp with time zone,
    revoked_at timestamp with time zone,
    rotated_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    project_id uuid,
    budget_group_id uuid,
    limit_lookup_id text,
    workload_issuer_id uuid,
    workload_digest text,
    workload_mapping text,
    paused_at timestamp with time zone,
    paused_authority_sequence bigint,
    CONSTRAINT api_key_workload_identity CHECK (((workload_issuer_id IS NULL) = (workload_digest IS NULL))),
    CONSTRAINT api_key_workload_mapping CHECK (((workload_issuer_id IS NULL) = (workload_mapping IS NULL))),
    CONSTRAINT api_keys_workload_digest_check CHECK ((workload_digest ~ '^[0-9a-f]{64}$'::text))
);

CREATE TABLE olp.projects (
    id uuid NOT NULL,
    name text NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    end_user_policy jsonb,
    attribution_policy jsonb,
    route_groups jsonb DEFAULT '{}'::jsonb NOT NULL,
    budget_policy jsonb,
    limit_templates jsonb DEFAULT '{}'::jsonb NOT NULL,
    attribution_budgets jsonb DEFAULT '{}'::jsonb NOT NULL,
    organization_id uuid,
    CONSTRAINT project_budget_policy_object CHECK (((budget_policy IS NULL) OR (jsonb_typeof(budget_policy) = 'object'::text))),
    CONSTRAINT projects_attribution_budgets_check CHECK ((jsonb_typeof(attribution_budgets) = 'object'::text)),
    CONSTRAINT projects_attribution_policy_check CHECK (((attribution_policy IS NULL) OR (jsonb_typeof(attribution_policy) = 'object'::text))),
    CONSTRAINT projects_end_user_policy_check CHECK (((end_user_policy IS NULL) OR (jsonb_typeof(end_user_policy) = 'object'::text))),
    CONSTRAINT projects_limit_templates_check CHECK ((jsonb_typeof(limit_templates) = 'object'::text)),
    CONSTRAINT projects_route_groups_check CHECK ((jsonb_typeof(route_groups) = 'object'::text))
);

CREATE VIEW olp.api_keys_with_limits AS
 SELECT k.id,
    k.lookup_id,
    k.digest,
    k.name,
    k.created_by,
    k.policy,
    k.etag,
    k.expires_at,
    k.revoked_at,
    k.rotated_at,
    k.created_at,
    k.project_id,
    k.budget_group_id,
    k.limit_lookup_id,
    k.workload_issuer_id,
    k.workload_digest,
    k.workload_mapping,
    k.paused_at,
    k.paused_authority_sequence,
    olp.effective_limits(k.policy, (p.limit_templates -> (k.policy ->> 'limit_template'::text))) AS effective_limits
   FROM (olp.api_keys k
     LEFT JOIN olp.projects p ON ((p.id = k.project_id)));

CREATE TABLE olp.async_worker_counters (
    singleton boolean DEFAULT true NOT NULL,
    request_metadata_reclaimed_total bigint DEFAULT 0 NOT NULL,
    request_metadata_recovered_total bigint DEFAULT 0 NOT NULL,
    request_metadata_duplicates_total bigint DEFAULT 0 CONSTRAINT async_worker_counters_request_metadata_duplicates_tota_not_null NOT NULL,
    request_metadata_processed_total bigint DEFAULT 0 NOT NULL,
    media_reconciliation_gaps_total bigint DEFAULT 0 NOT NULL,
    CONSTRAINT async_worker_counters_media_reconciliation_gaps_total_check CHECK ((media_reconciliation_gaps_total >= 0)),
    CONSTRAINT async_worker_counters_request_metadata_duplicates_total_check CHECK ((request_metadata_duplicates_total >= 0)),
    CONSTRAINT async_worker_counters_request_metadata_processed_total_check CHECK ((request_metadata_processed_total >= 0)),
    CONSTRAINT async_worker_counters_request_metadata_reclaimed_total_check CHECK ((request_metadata_reclaimed_total >= 0)),
    CONSTRAINT async_worker_counters_request_metadata_recovered_total_check CHECK ((request_metadata_recovered_total >= 0)),
    CONSTRAINT async_worker_counters_singleton_check CHECK (singleton)
);

CREATE TABLE olp.attempts (
    id uuid NOT NULL,
    request_id uuid NOT NULL,
    request_started_at timestamp with time zone NOT NULL,
    ordinal smallint NOT NULL,
    provider_id uuid NOT NULL,
    upstream_model text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    status_code integer,
    error_class text,
    committed boolean DEFAULT false NOT NULL,
    latency_ms integer,
    first_byte_ms integer,
    routing jsonb,
    guardrail_decisions jsonb DEFAULT '[]'::jsonb NOT NULL,
    provider_request_id text,
    CONSTRAINT attempts_first_byte_ms_check CHECK (((first_byte_ms IS NULL) OR (first_byte_ms >= 0))),
    CONSTRAINT attempts_guardrail_decisions_check CHECK (((jsonb_typeof(guardrail_decisions) = 'array'::text) AND (octet_length((guardrail_decisions)::text) <= 262144))),
    CONSTRAINT attempts_latency_ms_check CHECK (((latency_ms IS NULL) OR (latency_ms >= 0))),
    CONSTRAINT attempts_ordinal_check CHECK ((ordinal > 0)),
    CONSTRAINT attempts_status_code_check CHECK (((status_code IS NULL) OR ((status_code >= 100) AND (status_code <= 599))))
);

CREATE TABLE olp.audit (
    id uuid NOT NULL,
    actor_user_id uuid,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text,
    outcome text NOT NULL,
    source_ip text,
    user_agent_family text,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    actor_management_token_id uuid,
    project_id uuid,
    CONSTRAINT audit_one_actor_check CHECK ((NOT ((actor_user_id IS NOT NULL) AND (actor_management_token_id IS NOT NULL)))),
    CONSTRAINT audit_outcome_check CHECK ((outcome = ANY (ARRAY['success'::text, 'failure'::text])))
);

CREATE TABLE olp.auth_admission (
    action text NOT NULL,
    digest bytea NOT NULL,
    window_started_at timestamp with time zone DEFAULT now() NOT NULL,
    attempts integer NOT NULL
);

CREATE TABLE olp.batch_pending_recorded_owners (
    resource_id uuid NOT NULL,
    ordinal integer NOT NULL,
    budget_owner_id uuid NOT NULL
);

CREATE TABLE olp.batch_settlement_results (
    resource_id uuid NOT NULL,
    custom_id_digest bytea NOT NULL,
    result_kind text NOT NULL,
    event_id uuid NOT NULL,
    evidence_digest bytea NOT NULL,
    settled_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT batch_settlement_results_result_kind_check CHECK ((result_kind = ANY (ARRAY['succeeded'::text, 'errored'::text, 'cancelled'::text, 'expired'::text, 'aggregate'::text, 'gap'::text])))
);

CREATE TABLE olp.budget_calendar_changes (
    window_kind text NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    time_zone text NOT NULL,
    CONSTRAINT budget_calendar_changes_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text])))
);

CREATE TABLE olp.budget_group_cost_windows (
    budget_group_id uuid NOT NULL,
    window_kind text NOT NULL,
    window_id bigint NOT NULL,
    accrued numeric(28,12) NOT NULL,
    unpriced_attempts bigint NOT NULL,
    weekly_complete boolean DEFAULT false NOT NULL,
    charged numeric(28,12) DEFAULT 0 NOT NULL,
    uncharged_attempts bigint DEFAULT 0 NOT NULL,
    CONSTRAINT budget_group_cost_windows_accrued_check CHECK ((accrued >= (0)::numeric)),
    CONSTRAINT budget_group_cost_windows_charged_check CHECK ((charged >= (0)::numeric)),
    CONSTRAINT budget_group_cost_windows_check CHECK (((window_kind = 'month'::text) OR (unpriced_attempts = 0))),
    CONSTRAINT budget_group_cost_windows_uncharged_attempts_check CHECK ((uncharged_attempts >= 0)),
    CONSTRAINT budget_group_cost_windows_uncharged_scope_check CHECK (((window_kind = 'month'::text) OR (uncharged_attempts = 0))),
    CONSTRAINT budget_group_cost_windows_unpriced_attempts_check CHECK ((unpriced_attempts >= 0)),
    CONSTRAINT budget_group_cost_windows_window_id_check CHECK ((window_id >= 0)),
    CONSTRAINT budget_group_cost_windows_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text])))
);

CREATE TABLE olp.budget_groups (
    id uuid NOT NULL,
    name text NOT NULL,
    project_id uuid,
    daily_cost_limit numeric(24,12),
    monthly_cost_limit numeric(24,12),
    etag uuid NOT NULL,
    created_by uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    limit_template text,
    requests_per_minute bigint,
    tokens_per_minute bigint,
    max_concurrency bigint,
    weekly_cost_limit numeric(24,12),
    retired_at timestamp with time zone,
    cost_basis text DEFAULT 'cost'::text NOT NULL,
    CONSTRAINT budget_group_policy_lifetime CHECK ((((retired_at IS NULL) AND (created_by IS NOT NULL) AND ((limit_template IS NOT NULL) OR (daily_cost_limit IS NOT NULL) OR (weekly_cost_limit IS NOT NULL) OR (monthly_cost_limit IS NOT NULL) OR (requests_per_minute IS NOT NULL) OR (tokens_per_minute IS NOT NULL) OR (max_concurrency IS NOT NULL))) OR ((retired_at IS NOT NULL) AND (limit_template IS NULL) AND (daily_cost_limit IS NULL) AND (weekly_cost_limit IS NULL) AND (monthly_cost_limit IS NULL) AND (requests_per_minute IS NULL) AND (tokens_per_minute IS NULL) AND (max_concurrency IS NULL)))),
    CONSTRAINT budget_groups_cost_basis_check CHECK ((cost_basis = ANY (ARRAY['cost'::text, 'charge'::text]))),
    CONSTRAINT budget_groups_daily_cost_limit_check CHECK (((daily_cost_limit IS NULL) OR (daily_cost_limit > (0)::numeric))),
    CONSTRAINT budget_groups_max_concurrency_check CHECK (((max_concurrency >= 1) AND (max_concurrency <= 2147483647))),
    CONSTRAINT budget_groups_monthly_cost_limit_check CHECK (((monthly_cost_limit IS NULL) OR (monthly_cost_limit > (0)::numeric))),
    CONSTRAINT budget_groups_requests_per_minute_check CHECK (((requests_per_minute >= 1) AND (requests_per_minute <= 2147483647))),
    CONSTRAINT budget_groups_tokens_per_minute_check CHECK (((tokens_per_minute >= 1) AND (tokens_per_minute <= '9007199254740991'::bigint))),
    CONSTRAINT budget_groups_weekly_cost_limit_check CHECK ((weekly_cost_limit > (0)::numeric))
);

CREATE VIEW olp.budget_groups_with_limits AS
 SELECT g.id,
    g.name,
    g.project_id,
    g.daily_cost_limit,
    g.monthly_cost_limit,
    g.etag,
    g.created_by,
    g.created_at,
    g.updated_at,
    g.limit_template,
    g.requests_per_minute,
    g.tokens_per_minute,
    g.max_concurrency,
    g.weekly_cost_limit,
    olp.effective_limits(to_jsonb(g.*), (p.limit_templates -> g.limit_template)) AS effective_limits,
    g.retired_at,
    g.cost_basis
   FROM (olp.budget_groups g
     LEFT JOIN olp.projects p ON ((p.id = g.project_id)))
  WHERE (g.retired_at IS NULL);

CREATE TABLE olp.budget_increase_requests (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    owner_id uuid NOT NULL,
    window_kind text NOT NULL,
    window_id bigint NOT NULL,
    window_ends_at timestamp with time zone NOT NULL,
    base_cap numeric(24,12) NOT NULL,
    amount numeric(24,12) NOT NULL,
    reason text NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    requested_by uuid NOT NULL,
    decided_by uuid,
    decided_at timestamp with time zone,
    increase_id uuid,
    etag uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT budget_increase_requests_amount_check CHECK ((amount > (0)::numeric)),
    CONSTRAINT budget_increase_requests_base_cap_check CHECK ((base_cap > (0)::numeric)),
    CONSTRAINT budget_increase_requests_check CHECK (((status = 'pending'::text) = (decided_at IS NULL))),
    CONSTRAINT budget_increase_requests_check1 CHECK (((status = 'pending'::text) = (decided_by IS NULL))),
    CONSTRAINT budget_increase_requests_check2 CHECK (((status = 'approved'::text) = (increase_id IS NOT NULL))),
    CONSTRAINT budget_increase_requests_check3 CHECK (((status <> 'approved'::text) OR (decided_by <> requested_by))),
    CONSTRAINT budget_increase_requests_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 256))),
    CONSTRAINT budget_increase_requests_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'approved'::text, 'declined'::text]))),
    CONSTRAINT budget_increase_requests_window_id_check CHECK ((window_id >= 0)),
    CONSTRAINT budget_increase_requests_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text])))
);

CREATE TABLE olp.budget_increases (
    id uuid NOT NULL,
    owner_id uuid NOT NULL,
    project_id uuid,
    target jsonb NOT NULL,
    window_kind text NOT NULL,
    window_id bigint NOT NULL,
    amount numeric(24,12) NOT NULL,
    reason text NOT NULL,
    starts_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    window_ends_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by uuid NOT NULL,
    revoked_at timestamp with time zone,
    revoked_by uuid,
    etag uuid NOT NULL,
    organization_id uuid,
    CONSTRAINT budget_increases_amount_check CHECK ((amount > (0)::numeric)),
    CONSTRAINT budget_increases_check CHECK ((expires_at > starts_at)),
    CONSTRAINT budget_increases_check1 CHECK ((expires_at <= window_ends_at)),
    CONSTRAINT budget_increases_reason_check CHECK (((length(reason) >= 1) AND (length(reason) <= 256))),
    CONSTRAINT budget_increases_target_check CHECK ((jsonb_typeof(target) = 'object'::text)),
    CONSTRAINT budget_increases_window_id_check CHECK ((window_id >= 0)),
    CONSTRAINT budget_increases_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text])))
);

CREATE TABLE olp.capture_configuration (
    singleton boolean DEFAULT true NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    etag uuid DEFAULT uuidv7() NOT NULL,
    updated_by uuid,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT capture_configuration_singleton_check CHECK (singleton)
);

CREATE TABLE olp.capture_policies (
    id uuid NOT NULL,
    project_id uuid,
    route_slug text,
    sink_id uuid NOT NULL,
    sample_ratio text NOT NULL,
    include text[] NOT NULL,
    redact text[] DEFAULT '{}'::text[] NOT NULL,
    key_ids uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    end_user_digests text[] DEFAULT '{}'::text[] NOT NULL,
    max_bytes integer NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone,
    CONSTRAINT capture_policies_check CHECK (((project_id IS NOT NULL) OR (route_slug IS NOT NULL))),
    CONSTRAINT capture_policies_end_user_digests_check CHECK ((cardinality(end_user_digests) <= 256)),
    CONSTRAINT capture_policies_include_check CHECK (((cardinality(include) >= 1) AND (cardinality(include) <= 3) AND (include <@ ARRAY['input'::text, 'output'::text, 'tool_calls'::text]))),
    CONSTRAINT capture_policies_key_ids_check CHECK ((cardinality(key_ids) <= 256)),
    CONSTRAINT capture_policies_max_bytes_check CHECK (((max_bytes >= 1024) AND (max_bytes <= 1048576))),
    CONSTRAINT capture_policies_redact_check CHECK ((cardinality(redact) <= 128)),
    CONSTRAINT capture_policies_sample_ratio_check CHECK ((sample_ratio ~ '^(0(\.[0-9]{1,9})?|1(\.0{1,9})?)$'::text))
);

CREATE TABLE olp.capture_redactors (
    policy_id uuid NOT NULL,
    "position" integer NOT NULL,
    name text NOT NULL,
    revision_id uuid NOT NULL,
    CONSTRAINT capture_redactors_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100))),
    CONSTRAINT capture_redactors_position_check CHECK ((("position" >= 0) AND ("position" <= 127)))
);

CREATE TABLE olp.code_account_cooldowns (
    account_id uuid NOT NULL,
    model text,
    class text NOT NULL,
    retry_at timestamp with time zone NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    CONSTRAINT code_account_cooldowns_class_check CHECK ((class = ANY (ARRAY['quota'::text, 'entitlement'::text, 'rate'::text, 'capacity'::text, 'concurrency'::text]))),
    CONSTRAINT code_account_cooldowns_model_check CHECK ((model <> ''::text))
);

CREATE TABLE olp.code_attempts (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    route_id uuid NOT NULL,
    route_revision_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    binding_id uuid,
    account_id uuid,
    operation text NOT NULL,
    model text NOT NULL,
    state text NOT NULL,
    reserved_tokens bigint NOT NULL,
    reported_tokens bigint,
    input_tokens bigint,
    output_tokens bigint,
    cached_tokens bigint,
    reasoning_tokens bigint,
    bound_evidence text,
    refusal text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    finished_at timestamp with time zone,
    upstream_status integer,
    outcome_origin text,
    outcome text,
    outcome_observed_at timestamp with time zone,
    end_user_digest text DEFAULT ''::text NOT NULL,
    attribution jsonb DEFAULT '{}'::jsonb NOT NULL,
    nonexecution boolean DEFAULT false NOT NULL,
    initial_recovery boolean DEFAULT false NOT NULL,
    recovery_from uuid,
    requested_model text,
    paid_binding_id uuid,
    paid_provider_id uuid,
    paid_slot_id uuid,
    paid_cost_owners uuid[] DEFAULT '{}'::uuid[] NOT NULL,
    allowance_charge jsonb,
    CONSTRAINT code_attempt_target CHECK ((((binding_id IS NOT NULL) AND (account_id IS NOT NULL) AND (paid_binding_id IS NULL) AND (paid_provider_id IS NULL) AND (paid_slot_id IS NULL)) OR ((binding_id IS NULL) AND (account_id IS NULL) AND (paid_binding_id IS NOT NULL) AND (paid_provider_id IS NOT NULL) AND (paid_slot_id IS NOT NULL)))),
    CONSTRAINT code_attempts_allowance_charge_check CHECK ((jsonb_typeof(allowance_charge) = 'object'::text)),
    CONSTRAINT code_attempts_attribution_check CHECK ((jsonb_typeof(attribution) = 'object'::text)),
    CONSTRAINT code_attempts_end_user_digest_check CHECK (((end_user_digest = ''::text) OR (end_user_digest ~ '^[0-9a-f]{64}$'::text))),
    CONSTRAINT code_attempts_model_check CHECK (((octet_length(model) >= 1) AND (octet_length(model) <= 200))),
    CONSTRAINT code_attempts_operation_check CHECK (((octet_length(operation) >= 1) AND (octet_length(operation) <= 100))),
    CONSTRAINT code_attempts_outcome_check CHECK ((outcome = ANY (ARRAY['headers'::text, 'completed'::text, 'incomplete'::text, 'failed'::text, 'rejected'::text, 'interrupted'::text, 'canceled'::text, 'transport_error'::text]))),
    CONSTRAINT code_attempts_outcome_origin_check CHECK ((outcome_origin = ANY (ARRAY['upstream'::text, 'gateway'::text, 'client'::text]))),
    CONSTRAINT code_attempts_reported_tokens_check CHECK ((reported_tokens >= 0)),
    CONSTRAINT code_attempts_requested_model_check CHECK (((octet_length(requested_model) >= 1) AND (octet_length(requested_model) <= 200))),
    CONSTRAINT code_attempts_reserved_tokens_check CHECK ((reserved_tokens >= 0)),
    CONSTRAINT code_attempts_state_check CHECK ((state = ANY (ARRAY['prepared'::text, 'uncertain'::text, 'settled'::text, 'aborted'::text, 'bound_violation'::text]))),
    CONSTRAINT code_attempts_upstream_status_check CHECK (((upstream_status >= 100) AND (upstream_status <= 599))),
    CONSTRAINT code_outcome_observation CHECK ((((outcome_origin IS NULL) AND (outcome IS NULL) AND (outcome_observed_at IS NULL) AND (upstream_status IS NULL)) OR ((outcome_origin IS NOT NULL) AND (outcome IS NOT NULL) AND (outcome_observed_at IS NOT NULL))))
);

CREATE TABLE olp.code_bindings (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    route_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    conversation text NOT NULL,
    parent_id uuid,
    root_id uuid NOT NULL,
    account_id uuid NOT NULL,
    principal text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_at timestamp with time zone,
    socket_bound boolean DEFAULT true NOT NULL,
    CONSTRAINT code_bindings_conversation_check CHECK (((octet_length(conversation) >= 1) AND (octet_length(conversation) <= 256)))
);

CREATE TABLE olp.code_decisions (
    id uuid NOT NULL,
    attempt_id uuid,
    project_id uuid NOT NULL,
    route_id uuid NOT NULL,
    route_revision_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    binding_id uuid NOT NULL,
    root_id uuid NOT NULL,
    chosen_account_id uuid NOT NULL,
    reason text NOT NULL,
    skipped jsonb DEFAULT '[]'::jsonb NOT NULL,
    recovery_envelope text,
    recovery_ordinal integer,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT code_decisions_reason_check CHECK ((reason = ANY (ARRAY['chosen'::text, 'pin'::text, 'connection'::text]))),
    CONSTRAINT code_decisions_recovery_envelope_check CHECK ((recovery_envelope ~ '^[a-z0-9][a-z0-9_]{0,99}$'::text)),
    CONSTRAINT code_decisions_recovery_ordinal_check CHECK ((recovery_ordinal >= 1)),
    CONSTRAINT code_decisions_skipped_check CHECK ((jsonb_typeof(skipped) = 'array'::text))
);

CREATE TABLE olp.code_paid_bindings (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    route_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    conversation text NOT NULL,
    parent_id uuid,
    root_id uuid NOT NULL,
    model text NOT NULL,
    selected_model text NOT NULL,
    provider_id uuid NOT NULL,
    slot_id uuid NOT NULL,
    credential_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    retired_at timestamp with time zone,
    subscription_root_id uuid
);

CREATE TABLE olp.code_paid_references (
    route_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    external_id text NOT NULL,
    binding_id uuid NOT NULL,
    ambiguous boolean DEFAULT false NOT NULL
);

CREATE TABLE olp.code_pins (
    seq bigint NOT NULL,
    root_id uuid NOT NULL,
    model text NOT NULL,
    account_id uuid NOT NULL,
    principal text NOT NULL,
    adapter text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    target_model text,
    CONSTRAINT code_pins_model_check CHECK (((octet_length(model) >= 1) AND (octet_length(model) <= 200))),
    CONSTRAINT code_pins_target_model_check CHECK (((octet_length(target_model) >= 1) AND (octet_length(target_model) <= 200)))
);

ALTER TABLE olp.code_pins ALTER COLUMN seq ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME olp.code_pins_seq_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);

CREATE TABLE olp.code_pool_accounts (
    pool_id uuid NOT NULL,
    account_id uuid NOT NULL,
    weight bigint DEFAULT 1 NOT NULL,
    CONSTRAINT code_pool_accounts_weight_check CHECK (((weight >= 1) AND (weight <= 2147483647)))
);

CREATE TABLE olp.code_pool_keys (
    pool_id uuid NOT NULL,
    api_key_id uuid NOT NULL
);

CREATE TABLE olp.code_pools (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    owner_user_id uuid,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    CONSTRAINT code_pools_check CHECK (((kind = 'personal'::text) = (owner_user_id IS NOT NULL))),
    CONSTRAINT code_pools_kind_check CHECK ((kind = ANY (ARRAY['personal'::text, 'shared'::text])))
);

CREATE TABLE olp.code_references (
    route_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    external_id text NOT NULL,
    binding_id uuid NOT NULL,
    ambiguous boolean DEFAULT false NOT NULL,
    account_id uuid,
    CONSTRAINT code_references_external_id_check CHECK (((octet_length(external_id) >= 1) AND (octet_length(external_id) <= 256)))
);

CREATE TABLE olp.code_refusals (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    route_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    code text NOT NULL,
    occurred_at timestamp with time zone DEFAULT now() NOT NULL,
    end_user_digest text DEFAULT ''::text NOT NULL,
    CONSTRAINT code_refusals_code_check CHECK ((code ~ '^code_[a-z_]{1,80}$'::text)),
    CONSTRAINT code_refusals_end_user_digest_check CHECK (((end_user_digest = ''::text) OR (end_user_digest ~ '^[0-9a-f]{64}$'::text)))
);

CREATE TABLE olp.code_research_sessions (
    id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    revision_id uuid NOT NULL,
    service text NOT NULL,
    account_id uuid NOT NULL,
    credential_id uuid NOT NULL,
    generation bigint NOT NULL,
    upstream_id text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT code_research_sessions_generation_check CHECK ((generation > 0)),
    CONSTRAINT code_research_sessions_service_check CHECK ((service = ANY (ARRAY['search'::text, 'reader'::text, 'zread'::text]))),
    CONSTRAINT code_research_sessions_upstream_id_check CHECK (((octet_length(upstream_id) >= 0) AND (octet_length(upstream_id) <= 256)))
);

CREATE TABLE olp.code_route_revisions (
    id uuid NOT NULL,
    route_id uuid NOT NULL,
    revision integer NOT NULL,
    document jsonb NOT NULL,
    connections jsonb NOT NULL,
    published_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by uuid NOT NULL,
    paid_connections jsonb DEFAULT '{}'::jsonb NOT NULL
);

CREATE TABLE olp.code_routes (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    slug text NOT NULL,
    draft jsonb NOT NULL,
    etag uuid NOT NULL,
    latest_revision_id uuid,
    created_by uuid NOT NULL
);

CREATE TABLE olp.code_token_budgets (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    route_id uuid,
    api_key_id uuid,
    daily_tokens bigint,
    monthly_tokens bigint,
    enabled boolean DEFAULT true NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    CONSTRAINT code_token_budgets_check CHECK (((daily_tokens IS NOT NULL) OR (monthly_tokens IS NOT NULL))),
    CONSTRAINT code_token_budgets_daily_tokens_check CHECK (((daily_tokens >= 1) AND (daily_tokens <= '9007199254740991'::bigint))),
    CONSTRAINT code_token_budgets_monthly_tokens_check CHECK (((monthly_tokens >= 1) AND (monthly_tokens <= '9007199254740991'::bigint)))
);

CREATE TABLE olp.code_token_reservations (
    attempt_id uuid NOT NULL,
    budget_id uuid NOT NULL,
    period text NOT NULL,
    starts_at timestamp with time zone NOT NULL,
    tokens bigint NOT NULL,
    CONSTRAINT code_token_reservations_tokens_check CHECK ((tokens > 0))
);

CREATE TABLE olp.code_token_windows (
    budget_id uuid NOT NULL,
    period text NOT NULL,
    starts_at timestamp with time zone NOT NULL,
    reserved bigint DEFAULT 0 NOT NULL,
    measured bigint DEFAULT 0 NOT NULL,
    CONSTRAINT code_token_windows_measured_check CHECK ((measured >= 0)),
    CONSTRAINT code_token_windows_period_check CHECK ((period = ANY (ARRAY['day'::text, 'month'::text]))),
    CONSTRAINT code_token_windows_reserved_check CHECK ((reserved >= 0))
);

CREATE TABLE olp.commitment_periods (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    schedule_id uuid NOT NULL,
    provider_id uuid NOT NULL,
    period text NOT NULL,
    legacy boolean DEFAULT false NOT NULL,
    bucket_start timestamp with time zone NOT NULL,
    bucket_end timestamp with time zone NOT NULL,
    amount numeric(24,12) NOT NULL,
    complete boolean DEFAULT true NOT NULL,
    CONSTRAINT commitment_periods_amount_check CHECK ((amount > (0)::numeric)),
    CONSTRAINT commitment_periods_check CHECK ((bucket_end > bucket_start))
);

CREATE TABLE olp.commitment_usage (
    attempt_id uuid NOT NULL,
    period_id uuid NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    weight numeric(24,12) DEFAULT 0 NOT NULL,
    eligible boolean NOT NULL,
    complete boolean NOT NULL,
    payload jsonb NOT NULL
);

CREATE VIEW olp.commitment_allocations AS
 SELECT attempt_id,
    period_id,
    observed_at,
    weight,
    eligible,
    complete,
    payload,
    amount,
    total_weight,
    running_weight,
        CASE
            WHEN (total_weight > (0)::numeric) THEN (round(((amount * running_weight) / total_weight), 12) - round(((amount * (running_weight - weight)) / total_weight), 12))
            ELSE (0)::numeric
        END AS allocated_cost
   FROM ( SELECT u_1.attempt_id,
            u_1.period_id,
            u_1.observed_at,
            u_1.weight,
            u_1.eligible,
            u_1.complete,
            u_1.payload,
            p.amount,
            sum(u_1.weight) OVER (PARTITION BY u_1.period_id) AS total_weight,
            sum(u_1.weight) OVER (PARTITION BY u_1.period_id ORDER BY u_1.observed_at, u_1.attempt_id ROWS UNBOUNDED PRECEDING) AS running_weight
           FROM (olp.commitment_usage u_1
             JOIN olp.commitment_periods p ON ((p.id = u_1.period_id)))
          WHERE u_1.eligible) u;

CREATE TABLE olp.commitment_cutover (
    singleton boolean DEFAULT true NOT NULL,
    at timestamp with time zone NOT NULL,
    CONSTRAINT commitment_cutover_singleton_check CHECK (singleton)
);

CREATE TABLE olp.commitment_schedules (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    provider_id uuid NOT NULL,
    provider_revision_id uuid NOT NULL,
    provider_name text NOT NULL,
    provider_kind text NOT NULL,
    vendor_id text,
    project_id uuid,
    project_name text,
    amount numeric(24,12) NOT NULL,
    currency text NOT NULL,
    period text NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    ends_at timestamp with time zone,
    materialized_through timestamp with time zone,
    historical boolean DEFAULT false NOT NULL,
    CONSTRAINT commitment_schedules_amount_check CHECK ((amount > (0)::numeric)),
    CONSTRAINT commitment_schedules_check CHECK (((ends_at IS NULL) OR (ends_at > effective_at))),
    CONSTRAINT commitment_schedules_period_check CHECK ((period = ANY (ARRAY['hour'::text, 'day'::text, 'month'::text])))
);

CREATE TABLE olp.organization_members (
    organization_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role text NOT NULL,
    added_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT organization_members_role_check CHECK ((role = ANY (ARRAY['manager'::text, 'viewer'::text])))
);

CREATE TABLE olp.project_members (
    project_id uuid NOT NULL,
    user_id uuid NOT NULL,
    role text NOT NULL,
    added_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT project_members_role_check CHECK ((role = ANY (ARRAY['manager'::text, 'viewer'::text])))
);

CREATE TABLE olp.scim_group_members (
    group_id uuid NOT NULL,
    user_id uuid NOT NULL
);

CREATE TABLE olp.scim_group_projects (
    group_id uuid NOT NULL,
    project_id uuid NOT NULL,
    role text NOT NULL,
    CONSTRAINT scim_group_projects_role_check CHECK ((role = ANY (ARRAY['manager'::text, 'viewer'::text])))
);

CREATE TABLE olp.scim_users (
    user_id uuid NOT NULL,
    document jsonb NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT scim_users_document_check CHECK ((jsonb_typeof(document) = 'object'::text))
);

CREATE TABLE olp.users (
    id uuid NOT NULL,
    email text NOT NULL,
    display_name text NOT NULL,
    password_hash text,
    role text NOT NULL,
    active boolean DEFAULT true NOT NULL,
    etag uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    role_management text DEFAULT 'local'::text NOT NULL,
    oidc_authorized boolean DEFAULT true NOT NULL,
    access_scope text DEFAULT 'global'::text NOT NULL,
    mfa_revision uuid DEFAULT uuidv7() NOT NULL,
    CONSTRAINT users_access_scope_check CHECK ((access_scope = ANY (ARRAY['global'::text, 'assigned'::text]))),
    CONSTRAINT users_email_check CHECK ((email = lower(email))),
    CONSTRAINT users_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'operator'::text, 'developer'::text, 'viewer'::text]))),
    CONSTRAINT users_role_management_check CHECK ((role_management = ANY (ARRAY['local'::text, 'oidc'::text, 'saml'::text, 'provisioned'::text])))
);

CREATE VIEW olp.effective_project_members AS
 SELECT project_id,
    user_id,
        CASE
            WHEN bool_or((role = 'manager'::text)) THEN 'manager'::text
            ELSE 'viewer'::text
        END AS role
   FROM ( SELECT project_members.project_id,
            project_members.user_id,
            project_members.role
           FROM olp.project_members
        UNION ALL
         SELECT p.id,
            m.user_id,
            m.role
           FROM (olp.projects p
             JOIN olp.organization_members m ON ((m.organization_id = p.organization_id)))
        UNION ALL
         SELECT p.project_id,
            m.user_id,
            p.role
           FROM (((olp.scim_group_projects p
             JOIN olp.scim_group_members m ON ((m.group_id = p.group_id)))
             JOIN olp.scim_users su ON ((su.user_id = m.user_id)))
             JOIN olp.users u ON ((u.id = m.user_id)))
          WHERE ((su.deleted_at IS NULL) AND (u.role_management = 'provisioned'::text))) memberships
  GROUP BY project_id, user_id;

CREATE TABLE olp.end_user_accounts (
    id uuid NOT NULL,
    api_key_id uuid,
    project_id uuid,
    end_user_digest text NOT NULL,
    CONSTRAINT end_user_accounts_check CHECK (((api_key_id IS NULL) <> (project_id IS NULL))),
    CONSTRAINT end_user_accounts_end_user_digest_check CHECK ((end_user_digest ~ '^[0-9a-f]{64}$'::text))
);

CREATE TABLE olp.end_user_cost_windows (
    account_id uuid NOT NULL,
    window_kind text NOT NULL,
    window_id bigint NOT NULL,
    accrued numeric(28,12) NOT NULL,
    unpriced_attempts bigint NOT NULL,
    weekly_complete boolean DEFAULT false NOT NULL,
    charged numeric(28,12) DEFAULT 0 NOT NULL,
    uncharged_attempts bigint DEFAULT 0 NOT NULL,
    CONSTRAINT end_user_cost_windows_accrued_check CHECK ((accrued >= (0)::numeric)),
    CONSTRAINT end_user_cost_windows_charged_check CHECK ((charged >= (0)::numeric)),
    CONSTRAINT end_user_cost_windows_check CHECK (((window_kind = 'month'::text) OR (unpriced_attempts = 0))),
    CONSTRAINT end_user_cost_windows_uncharged_attempts_check CHECK ((uncharged_attempts >= 0)),
    CONSTRAINT end_user_cost_windows_uncharged_scope_check CHECK (((window_kind = 'month'::text) OR (uncharged_attempts = 0))),
    CONSTRAINT end_user_cost_windows_unpriced_attempts_check CHECK ((unpriced_attempts >= 0)),
    CONSTRAINT end_user_cost_windows_window_id_check CHECK ((window_id >= 0)),
    CONSTRAINT end_user_cost_windows_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text])))
);

CREATE TABLE olp.execution_debit_owners (
    dispatch_id uuid NOT NULL,
    owner_id uuid NOT NULL,
    owner_kind text NOT NULL,
    basis text NOT NULL,
    bound numeric(28,12) NOT NULL,
    daily_window_id bigint NOT NULL,
    weekly_window_id bigint NOT NULL,
    monthly_window_id bigint NOT NULL,
    CONSTRAINT execution_debit_owners_basis_check CHECK ((basis = ANY (ARRAY['cost'::text, 'charge'::text]))),
    CONSTRAINT execution_debit_owners_bound_check CHECK ((bound >= (0)::numeric)),
    CONSTRAINT execution_debit_owners_daily_window_id_check CHECK ((daily_window_id >= 0)),
    CONSTRAINT execution_debit_owners_monthly_window_id_check CHECK ((monthly_window_id >= 0)),
    CONSTRAINT execution_debit_owners_owner_kind_check CHECK ((owner_kind = ANY (ARRAY['key'::text, 'group'::text, 'aggregate'::text, 'end_user'::text, 'supply'::text]))),
    CONSTRAINT execution_debit_owners_weekly_window_id_check CHECK ((weekly_window_id >= 0))
);

CREATE TABLE olp.execution_debit_resolutions (
    dispatch_id uuid NOT NULL,
    actor_id uuid NOT NULL,
    evidence_digest bytea NOT NULL,
    created_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT execution_debit_resolutions_evidence_digest_check CHECK ((octet_length(evidence_digest) = 32))
);

CREATE TABLE olp.execution_debit_result_resolutions (
    dispatch_id uuid NOT NULL,
    resolved_digest bytea NOT NULL,
    evidence_result_digest bytea CONSTRAINT execution_debit_result_resoluti_evidence_result_digest_not_null NOT NULL,
    CONSTRAINT execution_debit_result_resolutions_check CHECK ((resolved_digest <> evidence_result_digest))
);

CREATE TABLE olp.execution_debit_results (
    dispatch_id uuid NOT NULL,
    result_digest bytea NOT NULL,
    evidence_digest bytea NOT NULL,
    resolution_digest bytea NOT NULL,
    event_id uuid NOT NULL,
    cost numeric(28,12) NOT NULL,
    charge numeric(28,12) NOT NULL,
    final boolean NOT NULL,
    complete boolean NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    usage_complete boolean DEFAULT false NOT NULL,
    CONSTRAINT execution_debit_results_charge_check CHECK ((charge >= (0)::numeric)),
    CONSTRAINT execution_debit_results_cost_check CHECK ((cost >= (0)::numeric)),
    CONSTRAINT execution_debit_results_evidence_digest_check CHECK ((octet_length(evidence_digest) = 32)),
    CONSTRAINT execution_debit_results_resolution_digest_check CHECK ((octet_length(resolution_digest) = 32)),
    CONSTRAINT execution_debit_results_result_digest_check CHECK ((octet_length(result_digest) = 32))
);

CREATE TABLE olp.execution_debit_versions (
    owner_id uuid NOT NULL,
    version bigint DEFAULT 0 NOT NULL,
    CONSTRAINT execution_debit_versions_version_check CHECK (((version >= 0) AND (version <= '9007199254740991'::bigint)))
);

CREATE TABLE olp.execution_debits (
    id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    project_id uuid,
    run_id uuid,
    step_id uuid,
    kind text NOT NULL,
    affinity jsonb NOT NULL,
    intent_digest bytea NOT NULL,
    claim_id uuid NOT NULL,
    currency text NOT NULL,
    cost_bound numeric(28,12) NOT NULL,
    charge_bound numeric(28,12) NOT NULL,
    observed_cost numeric(28,12) DEFAULT 0 NOT NULL,
    observed_charge numeric(28,12) DEFAULT 0 NOT NULL,
    state text DEFAULT 'prepared'::text NOT NULL,
    bound_violation boolean DEFAULT false NOT NULL,
    prepared_until timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    dispatched_at timestamp with time zone,
    closed_at timestamp with time zone,
    CONSTRAINT execution_debits_affinity_check CHECK (((jsonb_typeof(affinity) = 'object'::text) AND (octet_length((affinity)::text) <= 4096))),
    CONSTRAINT execution_debits_charge_bound_check CHECK ((charge_bound >= (0)::numeric)),
    CONSTRAINT execution_debits_check CHECK (((project_id IS NOT NULL) OR (kind = ANY (ARRAY['api'::text, 'model'::text])))),
    CONSTRAINT execution_debits_check1 CHECK (((closed_at IS NOT NULL) = (state = ANY (ARRAY['settled'::text, 'released'::text, 'not_accepted'::text])))),
    CONSTRAINT execution_debits_check2 CHECK (((state <> ALL (ARRAY['dispatched'::text, 'outcome_unknown'::text, 'settled'::text, 'not_accepted'::text, 'conflict'::text])) OR (dispatched_at IS NOT NULL))),
    CONSTRAINT execution_debits_check3 CHECK (((run_id IS NULL) = (step_id IS NULL))),
    CONSTRAINT execution_debits_cost_bound_check CHECK ((cost_bound >= (0)::numeric)),
    CONSTRAINT execution_debits_currency_check CHECK ((currency ~ '^[A-Z]{3}$'::text)),
    CONSTRAINT execution_debits_intent_digest_check CHECK ((octet_length(intent_digest) = 32)),
    CONSTRAINT execution_debits_kind_check CHECK ((kind = ANY (ARRAY['api'::text, 'model'::text, 'tool'::text, 'agent'::text]))),
    CONSTRAINT execution_debits_observed_charge_check CHECK ((observed_charge >= (0)::numeric)),
    CONSTRAINT execution_debits_observed_cost_check CHECK ((observed_cost >= (0)::numeric)),
    CONSTRAINT execution_debits_state_check CHECK ((state = ANY (ARRAY['prepared'::text, 'ready'::text, 'dispatched'::text, 'outcome_unknown'::text, 'settled'::text, 'released'::text, 'not_accepted'::text, 'conflict'::text])))
);

CREATE TABLE olp.supply_cost_windows (
    owner_id uuid NOT NULL,
    window_kind text NOT NULL,
    window_id bigint NOT NULL,
    accrued numeric(28,12) NOT NULL,
    unpriced_attempts bigint NOT NULL,
    weekly_complete boolean DEFAULT false NOT NULL,
    CONSTRAINT supply_cost_windows_accrued_check CHECK ((accrued >= (0)::numeric)),
    CONSTRAINT supply_cost_windows_unpriced_attempts_check CHECK ((unpriced_attempts >= 0)),
    CONSTRAINT supply_cost_windows_unpriced_scope_check CHECK (((window_kind = 'month'::text) OR (unpriced_attempts = 0))),
    CONSTRAINT supply_cost_windows_window_id_check CHECK ((window_id >= 0)),
    CONSTRAINT supply_cost_windows_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text])))
);

CREATE VIEW olp.execution_owner_spend AS
 SELECT api_key_cost_windows.api_key_id AS owner_id,
    api_key_cost_windows.window_kind,
    api_key_cost_windows.window_id,
    api_key_cost_windows.accrued,
    api_key_cost_windows.charged,
    api_key_cost_windows.unpriced_attempts,
    api_key_cost_windows.uncharged_attempts,
    api_key_cost_windows.weekly_complete
   FROM olp.api_key_cost_windows
UNION ALL
 SELECT budget_group_cost_windows.budget_group_id AS owner_id,
    budget_group_cost_windows.window_kind,
    budget_group_cost_windows.window_id,
    budget_group_cost_windows.accrued,
    budget_group_cost_windows.charged,
    budget_group_cost_windows.unpriced_attempts,
    budget_group_cost_windows.uncharged_attempts,
    budget_group_cost_windows.weekly_complete
   FROM olp.budget_group_cost_windows
UNION ALL
 SELECT aggregate_cost_windows.account_id AS owner_id,
    aggregate_cost_windows.window_kind,
    aggregate_cost_windows.window_id,
    aggregate_cost_windows.accrued,
    aggregate_cost_windows.charged,
    aggregate_cost_windows.unpriced_attempts,
    aggregate_cost_windows.uncharged_attempts,
    aggregate_cost_windows.weekly_complete
   FROM olp.aggregate_cost_windows
UNION ALL
 SELECT end_user_cost_windows.account_id AS owner_id,
    end_user_cost_windows.window_kind,
    end_user_cost_windows.window_id,
    end_user_cost_windows.accrued,
    end_user_cost_windows.charged,
    end_user_cost_windows.unpriced_attempts,
    end_user_cost_windows.uncharged_attempts,
    end_user_cost_windows.weekly_complete
   FROM olp.end_user_cost_windows
UNION ALL
 SELECT supply_cost_windows.owner_id,
    supply_cost_windows.window_kind,
    supply_cost_windows.window_id,
    supply_cost_windows.accrued,
    supply_cost_windows.accrued AS charged,
    supply_cost_windows.unpriced_attempts,
    supply_cost_windows.unpriced_attempts AS uncharged_attempts,
    supply_cost_windows.weekly_complete
   FROM olp.supply_cost_windows;

CREATE TABLE olp.export_cursors (
    sink_id uuid NOT NULL,
    stream text NOT NULL,
    cursor text,
    delivered_total bigint DEFAULT 0 NOT NULL,
    failed_total bigint DEFAULT 0 NOT NULL,
    gaps_total bigint DEFAULT 0 NOT NULL,
    last_success_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT export_cursors_delivered_total_check CHECK ((delivered_total >= 0)),
    CONSTRAINT export_cursors_failed_total_check CHECK ((failed_total >= 0)),
    CONSTRAINT export_cursors_gaps_total_check CHECK ((gaps_total >= 0))
);

CREATE TABLE olp.export_gaps (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    sink_id uuid NOT NULL,
    stream text NOT NULL,
    record_count bigint NOT NULL,
    first_occurred_at timestamp with time zone NOT NULL,
    last_occurred_at timestamp with time zone NOT NULL,
    reason text NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT export_gaps_reason_check CHECK ((reason = 'retention_expired'::text)),
    CONSTRAINT export_gaps_record_count_check CHECK ((record_count > 0))
);

CREATE TABLE olp.export_pending (
    sink_id uuid NOT NULL,
    record_id uuid NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    last_attempt_at timestamp with time zone,
    last_error_code text,
    delivered_at timestamp with time zone,
    rejected_at timestamp with time zone,
    delivery_settings jsonb,
    send_may_have_occurred boolean DEFAULT false NOT NULL,
    CONSTRAINT export_pending_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT export_pending_delivery_settings_check CHECK (((delivery_settings IS NULL) OR (jsonb_typeof(delivery_settings) = 'object'::text)))
);

CREATE TABLE olp.export_records (
    id uuid NOT NULL,
    stream text NOT NULL,
    source_id uuid NOT NULL,
    project_id uuid,
    route text DEFAULT ''::text NOT NULL,
    outcome text NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    queued_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    payload jsonb NOT NULL,
    CONSTRAINT export_records_payload_check CHECK ((octet_length((payload)::text) <= 1048576))
);

CREATE TABLE olp.focus_datasets (
    sink_id uuid NOT NULL,
    charge_date date NOT NULL,
    revision integer DEFAULT 0 NOT NULL,
    dirty_version bigint,
    generated_at timestamp with time zone,
    delivered_at timestamp with time zone,
    row_count bigint,
    part_count integer,
    billed_cost numeric(30,12),
    effective_cost numeric(30,12),
    attempts integer DEFAULT 0 NOT NULL,
    failures integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    last_error_code text,
    commitment_totals jsonb,
    CONSTRAINT focus_datasets_revision_check CHECK ((revision >= 0))
);

CREATE TABLE olp.focus_dirty_days (
    day date NOT NULL,
    version bigint NOT NULL
);

CREATE SEQUENCE olp.focus_dirty_version
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1;

CREATE TABLE olp.grant_enrollments (
    id uuid NOT NULL,
    provider_id uuid NOT NULL,
    slot_id uuid NOT NULL,
    plugin_digest text NOT NULL,
    profile_id text NOT NULL,
    started_by uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    continued_at timestamp with time zone,
    poll_interval integer,
    poll_at timestamp with time zone,
    outcome text,
    credential_id uuid,
    expected_credential_id uuid,
    CONSTRAINT grant_enrollments_completion_check CHECK ((((NOT (outcome IS DISTINCT FROM 'completed'::text)) = (credential_id IS NOT NULL)) AND ((outcome IS NULL) OR (continued_at IS NOT NULL)))),
    CONSTRAINT grant_enrollments_outcome_check CHECK ((outcome = ANY (ARRAY['completed'::text, 'denied'::text, 'expired'::text]))),
    CONSTRAINT grant_enrollments_poll_interval_check CHECK ((poll_interval > 0)),
    CONSTRAINT grant_enrollments_polling_check CHECK (((poll_interval IS NULL) = (poll_at IS NULL)))
);

CREATE TABLE olp.guardrail_attachments (
    id uuid NOT NULL,
    scope text NOT NULL,
    scope_id uuid,
    policy_revision_id uuid NOT NULL,
    "position" integer NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT guardrail_attachments_check CHECK (((scope = 'installation'::text) = (scope_id IS NULL))),
    CONSTRAINT guardrail_attachments_position_check CHECK (("position" >= 0)),
    CONSTRAINT guardrail_attachments_scope_check CHECK ((scope = ANY (ARRAY['installation'::text, 'organization'::text, 'project'::text, 'route'::text, 'toolset'::text, 'api-key'::text])))
);

CREATE TABLE olp.guardrail_credentials (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE olp.guardrail_draft_attachments (
    draft_id uuid NOT NULL,
    policy_revision_id uuid NOT NULL,
    "position" integer NOT NULL,
    created_by uuid NOT NULL,
    CONSTRAINT guardrail_draft_attachments_position_check CHECK (("position" >= 0))
);

CREATE TABLE olp.guardrail_policies (
    id uuid NOT NULL,
    project_id uuid,
    name text NOT NULL,
    latest_revision_id uuid NOT NULL,
    active_revision_id uuid,
    etag uuid NOT NULL,
    retired_at timestamp with time zone,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT guardrail_policies_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100)))
);

CREATE TABLE olp.guardrail_policy_revision_activations (
    revision_id uuid NOT NULL,
    activated_by uuid NOT NULL,
    activated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE olp.guardrail_policy_revisions (
    id uuid NOT NULL,
    policy_id uuid NOT NULL,
    revision bigint NOT NULL,
    entries jsonb NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT guardrail_policy_revisions_entries_check CHECK (((jsonb_typeof(entries) = 'array'::text) AND (jsonb_array_length(entries) <= 128))),
    CONSTRAINT guardrail_policy_revisions_revision_check CHECK ((revision > 0))
);

CREATE TABLE olp.guardrail_revision_activations (
    revision_id uuid NOT NULL,
    activated_by uuid NOT NULL,
    activated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE olp.guardrail_revisions (
    id uuid NOT NULL,
    guardrail_id uuid NOT NULL,
    revision bigint NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    configuration jsonb DEFAULT '{}'::jsonb NOT NULL,
    timeout_ms integer DEFAULT 1000 NOT NULL,
    on_error text DEFAULT 'fail_closed'::text NOT NULL,
    CONSTRAINT guardrail_revisions_on_error_check CHECK ((on_error = ANY (ARRAY['fail_closed'::text, 'fail_open'::text]))),
    CONSTRAINT guardrail_revisions_revision_check CHECK ((revision > 0)),
    CONSTRAINT guardrail_revisions_timeout_ms_check CHECK (((timeout_ms >= 1) AND (timeout_ms <= 300000)))
);

CREATE TABLE olp.guardrails (
    id uuid NOT NULL,
    project_id uuid,
    name text NOT NULL,
    type text NOT NULL,
    latest_revision_id uuid NOT NULL,
    etag uuid NOT NULL,
    retired_at timestamp with time zone,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    active_revision_id uuid,
    CONSTRAINT guardrails_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100))),
    CONSTRAINT guardrails_type_check CHECK ((type = ANY (ARRAY['builtin.regex'::text, 'builtin.pii'::text, 'builtin.secrets'::text, 'builtin.keywords'::text, 'builtin.schema'::text, 'builtin.tools'::text, 'builtin.code_execution'::text, 'builtin.tool_permission'::text, 'webhook'::text, 'bedrock'::text, 'azure.content_safety'::text, 'azure.prompt_shields'::text, 'model_armor'::text, 'lakera'::text, 'presidio'::text, 'model'::text, 'plugin'::text, 'aim'::text, 'akto'::text, 'alice'::text, 'aporia'::text, 'cato_networks'::text, 'cisco_ai_defense'::text, 'compresr'::text, 'conduct'::text, 'crowdstrike_aidr'::text, 'deepkeep'::text, 'dynamoai'::text, 'enkryptai'::text, 'generic_guardrail_api'::text, 'grayswan'::text, 'guardrails_ai'::text, 'headroom'::text, 'hiddenlayer'::text, 'ibm_guardrails'::text, 'javelin'::text, 'lakera_v1'::text, 'lasso'::text, 'microsoft_purview'::text, 'noma'::text, 'noma_v2'::text, 'onyx'::text, 'openai_moderation'::text, 'ovalix'::text, 'pangea'::text, 'panw_prisma_airs'::text, 'pillar'::text, 'prompt_security'::text, 'promptguard'::text, 'qostodian_nexus'::text, 'qualifire'::text, 'repelloai'::text, 'rubrik'::text, 'singulr'::text, 'straiker'::text, 'vigil_guard'::text, 'xecguard'::text, 'zscaler_ai_guard'::text, 'semantic_guard'::text, 'decision_model'::text, 'typesafe'::text])))
);

CREATE TABLE olp.installation (
    singleton boolean DEFAULT true NOT NULL,
    id uuid NOT NULL,
    name text DEFAULT 'OpenLLMProxy'::text NOT NULL,
    setup_complete boolean DEFAULT false NOT NULL,
    auth_fingerprint bytea,
    active_key_version integer,
    authority_id uuid NOT NULL,
    authority_sequence bigint DEFAULT 0 NOT NULL,
    release_sequence bigint DEFAULT 0 NOT NULL,
    budget_policy jsonb,
    budget_etag uuid DEFAULT uuidv7() NOT NULL,
    logo text DEFAULT ''::text NOT NULL,
    branding_etag uuid DEFAULT gen_random_uuid() NOT NULL,
    accent text DEFAULT ''::text NOT NULL,
    CONSTRAINT installation_accent_check CHECK (((accent = ''::text) OR (accent ~ '^#[0-9a-f]{6}$'::text))),
    CONSTRAINT installation_budget_policy_object CHECK (((budget_policy IS NULL) OR (jsonb_typeof(budget_policy) = 'object'::text))),
    CONSTRAINT installation_singleton_check CHECK (singleton)
);

CREATE TABLE olp.investigations (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    filters jsonb NOT NULL,
    evidence jsonb NOT NULL,
    summary text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE olp.invitations (
    id uuid NOT NULL,
    email text NOT NULL,
    role text NOT NULL,
    digest bytea NOT NULL,
    invited_by uuid NOT NULL,
    accepted_by uuid,
    revoked_by uuid,
    expires_at timestamp with time zone NOT NULL,
    accepted_at timestamp with time zone,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT invitations_role_check CHECK ((role = ANY (ARRAY['owner'::text, 'operator'::text, 'developer'::text, 'viewer'::text])))
);

CREATE TABLE olp.mcp_servers (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    transport text NOT NULL,
    endpoint text NOT NULL,
    enabled boolean NOT NULL,
    credential_id uuid,
    latest_revision_id uuid NOT NULL,
    etag uuid NOT NULL,
    retired_at timestamp with time zone,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    calls_per_minute integer DEFAULT 60 NOT NULL,
    max_concurrency integer DEFAULT 8 NOT NULL,
    per_key_calls_per_minute integer DEFAULT 60 NOT NULL,
    per_key_max_concurrency integer DEFAULT 8 NOT NULL,
    delegation_key_id uuid,
    entra_client_secret_id uuid,
    CONSTRAINT mcp_servers_calls_per_minute_check CHECK (((calls_per_minute >= 1) AND (calls_per_minute <= 60000))),
    CONSTRAINT mcp_servers_endpoint_check CHECK ((((transport = 'stdio'::text) AND (endpoint = ''::text)) OR ((transport <> 'stdio'::text) AND ((length(endpoint) >= 1) AND (length(endpoint) <= 2048))))),
    CONSTRAINT mcp_servers_max_concurrency_check CHECK (((max_concurrency >= 1) AND (max_concurrency <= 1024))),
    CONSTRAINT mcp_servers_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100))),
    CONSTRAINT mcp_servers_per_key_calls_per_minute_check CHECK (((per_key_calls_per_minute >= 1) AND (per_key_calls_per_minute <= 60000))),
    CONSTRAINT mcp_servers_per_key_max_concurrency_check CHECK (((per_key_max_concurrency >= 1) AND (per_key_max_concurrency <= 1024))),
    CONSTRAINT mcp_servers_transport_check CHECK ((transport = ANY (ARRAY['streamable_http'::text, 'sse'::text, 'stdio'::text])))
);

CREATE VIEW olp.live_agent_publication_revisions AS
 SELECT v.id,
    v.publication_id,
    v.project_id,
    v.kind,
    v.definition,
    v.digest,
    v.created_by,
    v.created_at
   FROM (olp.agent_publication_revisions v
     JOIN olp.agent_publications p ON ((p.id = v.publication_id)))
  WHERE ((NOT p.retired) AND ((v.kind <> 'toolset'::text) OR (NOT (EXISTS ( SELECT 1
           FROM (jsonb_array_elements(((v.definition -> 'toolset'::text) -> 'tools'::text)) t(value)
             LEFT JOIN olp.mcp_servers s ON ((s.id = ((t.value ->> 'server_id'::text))::uuid)))
          WHERE ((s.id IS NULL) OR (NOT s.enabled) OR (s.retired_at IS NOT NULL) OR (s.project_id <> v.project_id) OR (s.latest_revision_id <> ((t.value ->> 'revision_id'::text))::uuid)))))));

CREATE TABLE olp.managed_export_deliveries (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    sink_id uuid NOT NULL,
    event_id uuid NOT NULL,
    stream text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone DEFAULT (now() + '7 days'::interval) NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    status text DEFAULT 'pending'::text NOT NULL,
    lease uuid,
    last_error_code text,
    evidence_source_id uuid,
    evidence_project_id uuid,
    CONSTRAINT managed_export_deliveries_payload_check CHECK (((jsonb_typeof(payload) = 'object'::text) AND (octet_length((payload)::text) <= 65536))),
    CONSTRAINT managed_export_deliveries_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'delivered'::text, 'cancelled'::text, 'expired'::text])))
);

CREATE TABLE olp.managed_export_sinks (
    id uuid NOT NULL,
    project_id uuid,
    name text NOT NULL,
    type text NOT NULL,
    destination text NOT NULL,
    streams text[] NOT NULL,
    enabled boolean NOT NULL,
    credential_id uuid,
    etag uuid NOT NULL,
    retired_at timestamp with time zone,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    delivered_total bigint DEFAULT 0 NOT NULL,
    failed_total bigint DEFAULT 0 NOT NULL,
    expired_total bigint DEFAULT 0 NOT NULL,
    last_delivered_at timestamp with time zone,
    CONSTRAINT managed_export_sinks_check CHECK (((project_id IS NULL) OR (NOT ('audit'::text = ANY (streams))))),
    CONSTRAINT managed_export_sinks_destination_check CHECK (((length(destination) >= 1) AND (length(destination) <= 2048))),
    CONSTRAINT managed_export_sinks_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100))),
    CONSTRAINT managed_export_sinks_streams_check CHECK (((cardinality(streams) >= 1) AND (cardinality(streams) <= 4) AND (streams <@ ARRAY['requests'::text, 'attempts'::text, 'guardrail_decisions'::text, 'audit'::text, 'runs'::text, 'trace_spans'::text]))),
    CONSTRAINT managed_export_sinks_type_check CHECK ((type = 'https'::text))
);

CREATE TABLE olp.management_tokens (
    id uuid NOT NULL,
    lookup_id text NOT NULL,
    digest bytea NOT NULL,
    name text NOT NULL,
    scopes jsonb NOT NULL,
    created_by uuid NOT NULL,
    etag uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    all_projects boolean NOT NULL,
    project_ids jsonb NOT NULL
);

CREATE TABLE olp.mcp_call_identities (
    api_key_id uuid NOT NULL,
    project_id uuid NOT NULL,
    call_digest text NOT NULL,
    dispatch_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    toolset_id uuid NOT NULL,
    toolset_revision_id uuid NOT NULL,
    tool_alias text NOT NULL,
    input_digest text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT mcp_call_identities_call_digest_check CHECK ((call_digest ~ '^[a-f0-9]{64}$'::text)),
    CONSTRAINT mcp_call_identities_check CHECK ((expires_at > created_at)),
    CONSTRAINT mcp_call_identities_input_digest_check CHECK ((input_digest ~ '^[a-f0-9]{64}$'::text)),
    CONSTRAINT mcp_call_identities_tool_alias_check CHECK ((tool_alias ~ '^t_[a-f0-9]{32}_[a-f0-9]{32}$'::text))
);

CREATE TABLE olp.mcp_executable_permissions (
    executable text NOT NULL,
    digest text NOT NULL,
    approved_by uuid NOT NULL,
    approved_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT mcp_executable_permissions_digest_check CHECK ((digest ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT mcp_executable_permissions_executable_check CHECK ((executable ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'::text))
);

CREATE TABLE olp.mcp_grant_enrollments (
    id uuid NOT NULL,
    server_id uuid,
    project_id uuid NOT NULL,
    binding jsonb NOT NULL,
    binding_digest text NOT NULL,
    handoff_digest bytea NOT NULL,
    started_by uuid,
    claimed_by uuid,
    claimed_at timestamp with time zone,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    flow text,
    session_secret_id uuid,
    exchange_attempt_id uuid,
    outcome text,
    poll_at timestamp with time zone,
    poll_interval integer,
    agent_id uuid,
    CONSTRAINT mcp_grant_enrollments_binding_digest_check CHECK ((binding_digest ~ '^[a-f0-9]{64}$'::text)),
    CONSTRAINT mcp_grant_enrollments_check CHECK (((claimed_by IS NULL) = (claimed_at IS NULL))),
    CONSTRAINT mcp_grant_enrollments_flow_check CHECK ((flow = ANY (ARRAY['authorization_code'::text, 'device_code'::text]))),
    CONSTRAINT mcp_grant_enrollments_handoff_digest_check CHECK ((octet_length(handoff_digest) = 32)),
    CONSTRAINT mcp_grant_enrollments_outcome_check CHECK ((outcome = ANY (ARRAY['completed'::text, 'failed'::text, 'denied'::text, 'expired'::text]))),
    CONSTRAINT mcp_grant_enrollments_poll_interval_check CHECK (((poll_interval >= 1) AND (poll_interval <= 300))),
    CONSTRAINT mcp_grant_enrollments_resource CHECK ((num_nonnulls(server_id, agent_id) = 1))
);

CREATE TABLE olp.mcp_server_revisions (
    id uuid NOT NULL,
    server_id uuid NOT NULL,
    revision bigint NOT NULL,
    endpoint text NOT NULL,
    protocol_version text NOT NULL,
    tools jsonb NOT NULL,
    digest text NOT NULL,
    certified_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by uuid NOT NULL,
    unit_price numeric,
    tool_prices jsonb DEFAULT '{}'::jsonb NOT NULL,
    calls_per_minute integer DEFAULT 60 NOT NULL,
    max_concurrency integer DEFAULT 8 NOT NULL,
    per_key_calls_per_minute integer DEFAULT 60 NOT NULL,
    per_key_max_concurrency integer DEFAULT 8 NOT NULL,
    oauth jsonb,
    stdio jsonb,
    delegation jsonb,
    CONSTRAINT mcp_server_revisions_calls_per_minute_check CHECK (((calls_per_minute >= 1) AND (calls_per_minute <= 60000))),
    CONSTRAINT mcp_server_revisions_delegation_check CHECK (((delegation IS NULL) OR (jsonb_typeof(delegation) = 'object'::text))),
    CONSTRAINT mcp_server_revisions_digest_check CHECK ((digest ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT mcp_server_revisions_max_concurrency_check CHECK (((max_concurrency >= 1) AND (max_concurrency <= 1024))),
    CONSTRAINT mcp_server_revisions_per_key_calls_per_minute_check CHECK (((per_key_calls_per_minute >= 1) AND (per_key_calls_per_minute <= 60000))),
    CONSTRAINT mcp_server_revisions_per_key_max_concurrency_check CHECK (((per_key_max_concurrency >= 1) AND (per_key_max_concurrency <= 1024))),
    CONSTRAINT mcp_server_revisions_revision_check CHECK ((revision > 0)),
    CONSTRAINT mcp_server_revisions_tool_prices_check CHECK ((jsonb_typeof(tool_prices) = 'object'::text)),
    CONSTRAINT mcp_server_revisions_tools_check CHECK (((jsonb_typeof(tools) = 'array'::text) AND (jsonb_array_length(tools) <= 128) AND (octet_length((tools)::text) <= 1048576))),
    CONSTRAINT mcp_server_revisions_unit_price_check CHECK ((unit_price >= (0)::numeric))
);

CREATE TABLE olp.mcp_tool_calls (
    id uuid NOT NULL,
    dispatch_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    project_id uuid NOT NULL,
    toolset_id uuid NOT NULL,
    toolset_revision_id uuid NOT NULL,
    server_id uuid NOT NULL,
    server_revision_id uuid NOT NULL,
    tool_alias text NOT NULL,
    input_digest text NOT NULL,
    result_digest text,
    status text NOT NULL,
    is_error boolean DEFAULT false NOT NULL,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    call_digest text,
    unit_price numeric,
    pricing_state text DEFAULT 'unpriced'::text NOT NULL,
    currency text,
    guardrail_decisions jsonb DEFAULT '[]'::jsonb NOT NULL,
    identity_decisions jsonb DEFAULT '[]'::jsonb NOT NULL,
    agent_principal_id uuid,
    member_principal_id uuid,
    delegation_pair_digest text,
    CONSTRAINT mcp_tool_calls_call_digest_check CHECK ((call_digest ~ '^[a-f0-9]{64}$'::text)),
    CONSTRAINT mcp_tool_calls_check CHECK ((completed_at >= started_at)),
    CONSTRAINT mcp_tool_calls_check1 CHECK (((pricing_state = 'unpriced'::text) = (unit_price IS NULL))),
    CONSTRAINT mcp_tool_calls_currency_check CHECK ((currency ~ '^[A-Z]{3}$'::text)),
    CONSTRAINT mcp_tool_calls_delegation_pair CHECK ((((agent_principal_id IS NULL) AND (delegation_pair_digest IS NULL) AND (member_principal_id IS NULL)) OR ((agent_principal_id IS NOT NULL) AND (delegation_pair_digest IS NOT NULL) AND (delegation_pair_digest ~ '^[0-9a-f]{64}$'::text)))),
    CONSTRAINT mcp_tool_calls_guardrail_decisions_check CHECK (((jsonb_typeof(guardrail_decisions) = 'array'::text) AND (octet_length((guardrail_decisions)::text) <= 262144))),
    CONSTRAINT mcp_tool_calls_identity_decisions_check CHECK (((jsonb_typeof(identity_decisions) = 'array'::text) AND (octet_length((identity_decisions)::text) <= 4096))),
    CONSTRAINT mcp_tool_calls_input_digest_check CHECK ((input_digest ~ '^[a-f0-9]{64}$'::text)),
    CONSTRAINT mcp_tool_calls_pricing_state_check CHECK ((pricing_state = ANY (ARRAY['priced'::text, 'free'::text, 'unpriced'::text]))),
    CONSTRAINT mcp_tool_calls_result_digest_check CHECK ((result_digest ~ '^[a-f0-9]{64}$'::text)),
    CONSTRAINT mcp_tool_calls_status_check CHECK ((status = ANY (ARRAY['dispatched'::text, 'succeeded'::text, 'upstream_error'::text, 'outcome_unknown'::text, 'settled'::text, 'not_accepted'::text, 'refused_unpriced'::text, 'guardrail_blocked'::text]))),
    CONSTRAINT mcp_tool_calls_tool_alias_check CHECK ((tool_alias ~ '^t_[a-f0-9]{32}_[a-f0-9]{32}$'::text)),
    CONSTRAINT mcp_tool_calls_unit_price_check CHECK ((unit_price >= (0)::numeric))
);

CREATE TABLE olp.mcp_user_grants (
    id uuid NOT NULL,
    enrollment_id uuid NOT NULL,
    server_id uuid,
    project_id uuid NOT NULL,
    binding jsonb NOT NULL,
    binding_digest text NOT NULL,
    member_id uuid,
    access_secret_id uuid,
    expires_at timestamp with time zone NOT NULL,
    revoked_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    refresh_secret_id uuid,
    refresh_attempt_id uuid,
    failure_code text,
    agent_id uuid,
    CONSTRAINT mcp_user_grants_binding_digest_check CHECK ((binding_digest ~ '^[a-f0-9]{64}$'::text)),
    CONSTRAINT mcp_user_grants_failure_code_check CHECK ((failure_code = 'refresh_failed'::text)),
    CONSTRAINT mcp_user_grants_member_origin CHECK (((member_id IS NOT NULL) OR (((binding -> 'oauth'::text) -> 'entra_obo'::text) IS NOT NULL))),
    CONSTRAINT mcp_user_grants_resource CHECK ((num_nonnulls(server_id, agent_id) = 1))
);

CREATE TABLE olp.media_jobs (
    id uuid NOT NULL,
    upstream_job_id text,
    api_key_id uuid NOT NULL,
    provider_id uuid NOT NULL,
    provider_model text NOT NULL,
    route_slug text NOT NULL,
    operation text NOT NULL,
    surface text DEFAULT 'openai'::text NOT NULL,
    state text NOT NULL,
    lifecycle_state text DEFAULT 'active'::text NOT NULL,
    progress_percent numeric(5,2),
    content_available boolean DEFAULT false NOT NULL,
    expires_at timestamp with time zone,
    error_class text,
    completed_at timestamp with time zone,
    last_polled_at timestamp with time zone,
    reconciliation_error text,
    deleted_at timestamp with time zone,
    etag uuid NOT NULL,
    runtime_generation_id uuid NOT NULL,
    provider_revision_id uuid NOT NULL,
    credential_version_id uuid,
    reconciliation_claim_id uuid,
    reconciliation_claimed_until timestamp with time zone,
    reconciliation_attempts integer DEFAULT 0 NOT NULL,
    next_reconciliation_at timestamp with time zone DEFAULT now() NOT NULL,
    last_reconciliation_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    slot_id uuid NOT NULL,
    strict_contract boolean NOT NULL,
    native_source_id uuid,
    CONSTRAINT media_jobs_check CHECK ((((state = ANY (ARRAY['succeeded'::text, 'failed'::text, 'cancelled'::text])) AND (completed_at IS NOT NULL)) OR ((state = ANY (ARRAY['queued'::text, 'running'::text])) AND (completed_at IS NULL)))),
    CONSTRAINT media_jobs_check1 CHECK ((((lifecycle_state = 'deleted'::text) AND (deleted_at IS NOT NULL)) OR ((lifecycle_state <> 'deleted'::text) AND (deleted_at IS NULL)))),
    CONSTRAINT media_jobs_check2 CHECK ((((reconciliation_claim_id IS NULL) AND (reconciliation_claimed_until IS NULL)) OR ((reconciliation_claim_id IS NOT NULL) AND (reconciliation_claimed_until IS NOT NULL)))),
    CONSTRAINT media_jobs_check3 CHECK (((lifecycle_state = ANY (ARRAY['creating'::text, 'create_ambiguous'::text, 'deleted'::text])) OR (upstream_job_id IS NOT NULL))),
    CONSTRAINT media_jobs_lifecycle_state_check CHECK ((lifecycle_state = ANY (ARRAY['creating'::text, 'active'::text, 'create_ambiguous'::text, 'create_cleanup_pending'::text, 'delete_pending'::text, 'deleted'::text]))),
    CONSTRAINT media_jobs_progress_percent_check CHECK (((progress_percent IS NULL) OR ((progress_percent >= (0)::numeric) AND (progress_percent <= (100)::numeric)))),
    CONSTRAINT media_jobs_reconciliation_attempts_check CHECK ((reconciliation_attempts >= 0)),
    CONSTRAINT media_jobs_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'running'::text, 'succeeded'::text, 'failed'::text, 'cancelled'::text]))),
    CONSTRAINT media_jobs_surface_check CHECK ((surface = ANY (ARRAY['openai'::text, 'anthropic'::text, 'gemini'::text])))
);

CREATE TABLE olp.metering_delivery_receipts (
    sink_id uuid NOT NULL,
    attempt_id uuid NOT NULL,
    record_id uuid NOT NULL,
    project_id uuid,
    outcome text NOT NULL,
    last_error_code text,
    attempts integer DEFAULT 0 NOT NULL,
    occurred_at timestamp with time zone NOT NULL,
    queued_at timestamp with time zone NOT NULL,
    last_attempt_at timestamp with time zone,
    settled_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT metering_delivery_receipts_attempts_check CHECK ((attempts >= 0)),
    CONSTRAINT metering_delivery_receipts_outcome_check CHECK ((outcome = ANY (ARRAY['accepted'::text, 'delivery_unconfirmed'::text, 'expired_before_delivery'::text])))
);

CREATE TABLE olp.mfa_challenges (
    id uuid NOT NULL,
    digest bytea NOT NULL,
    user_id uuid NOT NULL,
    user_etag uuid NOT NULL,
    mfa_revision uuid NOT NULL,
    session_id uuid,
    purpose text NOT NULL,
    data jsonb NOT NULL,
    secret_id uuid,
    attempts integer DEFAULT 0 NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT mfa_challenges_attempts_check CHECK (((attempts >= 0) AND (attempts <= 5))),
    CONSTRAINT mfa_challenges_data_check CHECK ((jsonb_typeof(data) = 'object'::text)),
    CONSTRAINT mfa_challenges_digest_check CHECK ((octet_length(digest) = 32)),
    CONSTRAINT mfa_challenges_purpose_check CHECK ((purpose = ANY (ARRAY['login'::text, 'bootstrap'::text, 'manage'::text, 'reauth'::text, 'enroll_totp'::text, 'enroll_webauthn'::text])))
);

CREATE TABLE olp.mfa_factors (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    kind text NOT NULL,
    name text NOT NULL,
    credential_id bytea,
    last_counter bigint DEFAULT '-1'::integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_used_at timestamp with time zone,
    CONSTRAINT mfa_factors_check CHECK (((kind = 'webauthn'::text) = (credential_id IS NOT NULL))),
    CONSTRAINT mfa_factors_credential_id_check CHECK (((credential_id IS NULL) OR ((octet_length(credential_id) >= 1) AND (octet_length(credential_id) <= 1024)))),
    CONSTRAINT mfa_factors_kind_check CHECK ((kind = ANY (ARRAY['totp'::text, 'webauthn'::text]))),
    CONSTRAINT mfa_factors_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100)))
);

CREATE TABLE olp.mfa_recovery_codes (
    user_id uuid NOT NULL,
    digest bytea NOT NULL,
    CONSTRAINT mfa_recovery_codes_digest_check CHECK ((octet_length(digest) = 32))
);

CREATE TABLE olp.model_catalog_project_settings (
    project_id uuid NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    prices_public boolean DEFAULT false NOT NULL,
    etag uuid DEFAULT gen_random_uuid() NOT NULL
);

CREATE TABLE olp.model_catalog_route_settings (
    route_id uuid NOT NULL,
    expose_upstream_models boolean DEFAULT false NOT NULL,
    etag uuid DEFAULT gen_random_uuid() NOT NULL
);

CREATE TABLE olp.notification_deliveries (
    id uuid NOT NULL,
    rule_id uuid NOT NULL,
    window_id bigint,
    threshold_percent integer,
    accrued numeric(28,12),
    limit_amount numeric(24,12),
    currency character(3),
    status text NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    last_error_code text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_attempt_at timestamp with time zone,
    delivered_at timestamp with time zone,
    credential_id uuid,
    payload jsonb,
    api_key_id uuid,
    due_at timestamp with time zone,
    reason text,
    event text,
    dedup_key text,
    resolved boolean DEFAULT false NOT NULL,
    budget_subject_kind text,
    budget_subject_id uuid,
    budget_window_kind text,
    CONSTRAINT budget_alert_deliveries_accrued_check CHECK ((accrued >= (0)::numeric)),
    CONSTRAINT budget_alert_deliveries_attempts_check CHECK (((attempts >= 0) AND (attempts <= 5))),
    CONSTRAINT budget_alert_deliveries_limit_amount_check CHECK ((limit_amount > (0)::numeric)),
    CONSTRAINT notification_budget_subject_check CHECK ((((window_id IS NULL) AND (num_nonnulls(budget_subject_kind, budget_subject_id, budget_window_kind) = 0)) OR ((window_id IS NOT NULL) AND (num_nonnulls(budget_subject_kind, budget_subject_id, budget_window_kind) = 3) AND (budget_subject_kind = ANY (ARRAY['api_key'::text, 'budget_group'::text])) AND (budget_window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text]))))),
    CONSTRAINT notification_deliveries_dedup_key_check CHECK ((length(dedup_key) <= 512)),
    CONSTRAINT notification_deliveries_evidence_check CHECK ((((num_nonnulls(window_id, threshold_percent, accrued, limit_amount) = 4) AND (num_nonnulls(credential_id, payload, api_key_id, due_at, reason, event, dedup_key) = 0)) OR ((num_nonnulls(window_id, threshold_percent, accrued, limit_amount, currency, api_key_id, due_at, reason, event, dedup_key) = 0) AND (num_nonnulls(credential_id, payload) = 2)) OR ((num_nonnulls(window_id, threshold_percent, accrued, limit_amount, currency, credential_id, event, dedup_key) = 0) AND (num_nonnulls(api_key_id, due_at, reason, payload) = 4)) OR ((num_nonnulls(window_id, threshold_percent, accrued, limit_amount, currency, credential_id, api_key_id, due_at, reason) = 0) AND (num_nonnulls(event, dedup_key, payload) = 3)))),
    CONSTRAINT notification_deliveries_reason_check CHECK ((reason = ANY (ARRAY['expiry'::text, 'rotation'::text]))),
    CONSTRAINT notification_deliveries_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'delivered'::text, 'failed'::text, 'cancelled'::text])))
);

CREATE TABLE olp.notification_destination_tests (
    id uuid NOT NULL,
    destination_id uuid NOT NULL,
    action_id uuid NOT NULL,
    claimed_at timestamp with time zone NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    finished_at timestamp with time zone,
    status text DEFAULT 'unconfirmed'::text NOT NULL,
    failure text,
    CONSTRAINT notification_destination_tests_status_check CHECK ((status = ANY (ARRAY['delivered'::text, 'failed'::text, 'unconfirmed'::text])))
);

CREATE TABLE olp.notification_destinations (
    id uuid NOT NULL,
    name text NOT NULL,
    url text NOT NULL,
    project_id uuid,
    secret_id uuid,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    type text DEFAULT 'webhook'::text NOT NULL,
    configuration jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT notification_destinations_configuration_check CHECK (((jsonb_typeof(configuration) = 'object'::text) AND (octet_length((configuration)::text) <= 4096))),
    CONSTRAINT notification_destinations_type_check CHECK ((type = ANY (ARRAY['webhook'::text, 'slack'::text, 'msteams'::text, 'discord'::text, 'pagerduty'::text, 'email'::text])))
);

CREATE TABLE olp.notification_rules (
    id uuid NOT NULL,
    name text NOT NULL,
    project_id uuid,
    subject_kind text,
    subject_id uuid,
    window_kind text,
    threshold_percent integer,
    destination_id uuid NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    event text NOT NULL,
    configuration jsonb DEFAULT '{}'::jsonb NOT NULL,
    silence_starts_at timestamp with time zone,
    silence_ends_at timestamp with time zone,
    CONSTRAINT budget_alert_rules_subject_kind_check CHECK ((subject_kind = ANY (ARRAY['api_key'::text, 'budget_group'::text]))),
    CONSTRAINT budget_alert_rules_threshold_percent_check CHECK (((threshold_percent >= 1) AND (threshold_percent <= 100))),
    CONSTRAINT budget_alert_rules_window_kind_check CHECK ((window_kind = ANY (ARRAY['day'::text, 'week'::text, 'month'::text]))),
    CONSTRAINT notification_rules_configuration_check CHECK (((jsonb_typeof(configuration) = 'object'::text) AND (octet_length((configuration)::text) <= 4096))),
    CONSTRAINT notification_rules_event_check CHECK ((event = ANY (ARRAY['budget.threshold'::text, 'provider.grant.lapsed'::text, 'key.expiring'::text, 'budget.exhausted'::text, 'provider.circuit.open'::text, 'provider.circuit.closed'::text, 'provider.error_rate'::text, 'route.latency'::text, 'provider.credential.failing'::text, 'model.retirement'::text, 'runtime.install_failed'::text, 'worker.stale'::text, 'report.spend'::text, 'spend.spike'::text]))),
    CONSTRAINT notification_rules_silence_check CHECK (((num_nonnulls(silence_starts_at, silence_ends_at) = 0) OR ((num_nonnulls(silence_starts_at, silence_ends_at) = 2) AND (event = 'route.latency'::text) AND (project_id IS NOT NULL) AND isfinite(silence_starts_at) AND isfinite(silence_ends_at) AND (silence_starts_at < silence_ends_at)))),
    CONSTRAINT notification_rules_subject_check CHECK (
CASE event
    WHEN 'budget.threshold'::text THEN (num_nonnulls(subject_kind, subject_id, window_kind, threshold_percent) = 4)
    WHEN 'key.expiring'::text THEN ((NOT (subject_kind IS DISTINCT FROM 'api_key'::text)) AND (subject_id IS NOT NULL) AND (num_nonnulls(window_kind, threshold_percent) = 0))
    WHEN 'spend.spike'::text THEN ((project_id IS NOT NULL) AND (num_nonnulls(subject_kind, subject_id, window_kind, threshold_percent) = 0))
    ELSE ((num_nonnulls(subject_kind, subject_id, window_kind, threshold_percent) = 0) AND ((event = ANY (ARRAY['budget.exhausted'::text, 'route.latency'::text, 'report.spend'::text, 'model.retirement'::text])) OR (project_id IS NULL)))
END)
);

CREATE TABLE olp.notification_signal_states (
    rule_id uuid NOT NULL,
    subject text NOT NULL,
    active boolean DEFAULT false NOT NULL,
    incident bigint DEFAULT 0 NOT NULL,
    last_trigger_at timestamp with time zone,
    checked_at timestamp with time zone DEFAULT now() NOT NULL,
    trigger_pending boolean DEFAULT false NOT NULL,
    CONSTRAINT notification_signal_states_incident_check CHECK ((incident >= 0)),
    CONSTRAINT notification_signal_states_subject_check CHECK ((length(subject) <= 512))
);

CREATE TABLE olp.oidc_configuration (
    singleton boolean DEFAULT true NOT NULL,
    id uuid NOT NULL,
    document jsonb NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL,
    CONSTRAINT oidc_configuration_singleton_check CHECK (singleton)
);

CREATE TABLE olp.oidc_flows (
    id uuid NOT NULL,
    state_digest bytea NOT NULL,
    cookie_digest bytea NOT NULL,
    configuration_etag uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL
);

CREATE TABLE olp.oidc_identities (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    email_at_link text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_login_at timestamp with time zone,
    role_claims jsonb
);

CREATE TABLE olp.organizations (
    id uuid NOT NULL,
    name text NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    budget_policy jsonb,
    CONSTRAINT organizations_budget_policy_check CHECK (((budget_policy IS NULL) OR (jsonb_typeof(budget_policy) = 'object'::text))),
    CONSTRAINT organizations_name_check CHECK (((length(name) >= 1) AND (length(name) <= 100)))
);

CREATE TABLE olp.pending_resource_recorded_owners (
    resource_id uuid NOT NULL,
    ordinal integer NOT NULL,
    budget_owner_id uuid NOT NULL
);

CREATE TABLE olp.retained_settlements (
    resource_id uuid NOT NULL,
    operation text NOT NULL,
    templates jsonb NOT NULL,
    token_consuming boolean NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    started_at timestamp with time zone DEFAULT now() NOT NULL,
    remote_terminated boolean DEFAULT false NOT NULL,
    terminal_at timestamp with time zone,
    poll_after timestamp with time zone DEFAULT now() NOT NULL,
    claim_id uuid,
    lease_until timestamp with time zone,
    failures integer DEFAULT 0 NOT NULL,
    last_error text,
    reconciliation_hint text,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT retained_settlements_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'evidence_missing'::text, 'closed'::text]))),
    CONSTRAINT retained_settlements_templates_check CHECK ((jsonb_typeof(templates) = 'object'::text))
);

CREATE VIEW olp.provider_resource_pending_tokens AS
 SELECT p.resource_id,
    p.ordinal,
    p.api_key_id,
    p.budget_group_id,
    p.project_id,
    p.api_key_project_id,
    p.route_id,
    p.leg_route_id,
    p.pinned_route_id,
    p.budget_owner_ids,
    p.route_slug,
    p.provider_id,
    p.slot_id,
    p.attribution,
    p.end_user_digest
   FROM (olp.provider_resource_pending_usage p
     LEFT JOIN olp.retained_settlements s ON ((s.resource_id = p.resource_id)))
  WHERE ((s.resource_id IS NULL) OR s.token_consuming);

CREATE VIEW olp.pending_token_recorded_owners AS
 SELECT o.resource_id,
    o.ordinal,
    o.budget_owner_id
   FROM (olp.pending_resource_recorded_owners o
     JOIN olp.provider_resource_pending_tokens p USING (resource_id, ordinal));

CREATE TABLE olp.plugins (
    digest text NOT NULL,
    abi_version integer NOT NULL,
    manifest jsonb NOT NULL,
    module bytea,
    etag uuid NOT NULL,
    installed_by uuid NOT NULL,
    installed_at timestamp with time zone DEFAULT now() NOT NULL,
    approved_by uuid,
    approved_at timestamp with time zone,
    executable text,
    size_bytes bigint NOT NULL,
    CONSTRAINT plugins_approval_check CHECK (((approved_by IS NULL) = (approved_at IS NULL))),
    CONSTRAINT plugins_digest_check CHECK ((digest ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT plugins_executable_check CHECK ((executable ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'::text)),
    CONSTRAINT plugins_tier_check CHECK (((module IS NULL) = (executable IS NOT NULL)))
);

CREATE TABLE olp.prices (
    pricing_revision_id uuid NOT NULL,
    provider_kind text NOT NULL,
    model text NOT NULL,
    operation text NOT NULL,
    input_per_million numeric(24,12),
    output_per_million numeric(24,12),
    cached_input_per_million numeric(24,12),
    unit_price numeric(24,12),
    currency character(3) DEFAULT 'USD'::bpchar NOT NULL,
    provider_id uuid,
    vendor_id text,
    cache_write_input_per_million numeric(24,12),
    cache_write_5m_input_per_million numeric(24,12),
    cache_write_1h_input_per_million numeric(24,12),
    audio_input_per_million numeric(24,12),
    cached_audio_input_per_million numeric(24,12),
    audio_output_per_million numeric(24,12),
    image_input_per_million numeric(24,12),
    cached_image_input_per_million numeric(24,12),
    image_output_per_million numeric(24,12),
    per_request numeric(24,12),
    batch_multiplier numeric(24,12),
    tool_calls jsonb,
    tiers jsonb,
    windows jsonb,
    training_per_million numeric(24,12),
    storage_per_gib_day numeric(24,12),
    cache_storage_per_million_token_hour numeric(24,12),
    container_per_session jsonb,
    CONSTRAINT prices_cache_storage_per_million_token_hour_check CHECK ((cache_storage_per_million_token_hour >= (0)::numeric)),
    CONSTRAINT prices_currency_check CHECK ((((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text))),
    CONSTRAINT prices_operation_check CHECK ((operation ~ '^[a-z][a-z0-9_-]{0,95}$'::text)),
    CONSTRAINT prices_plugin_provider_check CHECK (((provider_kind <> 'plugin'::text) OR (provider_id IS NOT NULL))),
    CONSTRAINT prices_provider_kind_check CHECK ((provider_kind = ANY (ARRAY['openai'::text, 'anthropic'::text, 'gemini'::text, 'vertex_ai'::text, 'bedrock'::text, 'sagemaker'::text, 'watsonx'::text, 'azure_openai'::text, 'azure_ai_search'::text, 'openai_compatible'::text, 'plugin'::text]))),
    CONSTRAINT prices_storage_per_gib_day_check CHECK ((storage_per_gib_day >= (0)::numeric)),
    CONSTRAINT prices_tiers_check CHECK (((tiers IS NULL) OR ((jsonb_typeof(tiers) = 'array'::text) AND (octet_length((tiers)::text) <= 16384)))),
    CONSTRAINT prices_tool_calls_check CHECK (((tool_calls IS NULL) OR ((jsonb_typeof(tool_calls) = 'object'::text) AND (octet_length((tool_calls)::text) <= 4096)))),
    CONSTRAINT prices_training_per_million_check CHECK ((training_per_million >= (0)::numeric)),
    CONSTRAINT prices_windows_check CHECK (((windows IS NULL) OR ((jsonb_typeof(windows) = 'object'::text) AND (octet_length((windows)::text) <= 8192)))),
    CONSTRAINT retained_container_rates_object CHECK (((container_per_session IS NULL) OR (jsonb_typeof(container_per_session) = 'object'::text)))
);

CREATE TABLE olp.pricing_currency (
    singleton boolean DEFAULT true NOT NULL,
    currency character(3) NOT NULL,
    CONSTRAINT pricing_currency_currency_check CHECK ((((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text))),
    CONSTRAINT pricing_currency_singleton_check CHECK (singleton)
);

CREATE TABLE olp.pricing_discounts (
    id uuid NOT NULL,
    provider_kind text NOT NULL,
    vendor_id text,
    provider_id uuid,
    multiplier numeric(24,12) NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by uuid NOT NULL,
    CONSTRAINT pricing_discounts_provider_kind_check CHECK ((provider_kind = ANY (ARRAY['openai'::text, 'anthropic'::text, 'gemini'::text, 'vertex_ai'::text, 'bedrock'::text, 'sagemaker'::text, 'watsonx'::text, 'azure_openai'::text, 'azure_ai_search'::text, 'openai_compatible'::text, 'plugin'::text])))
);

CREATE TABLE olp.pricing_revisions (
    id uuid NOT NULL,
    revision integer NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    source_snapshot_id uuid
);

CREATE TABLE olp.pricing_source_snapshots (
    id uuid NOT NULL,
    source_id uuid NOT NULL,
    sha256 text NOT NULL,
    document jsonb NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL,
    catalog_sha256 text,
    catalog_published_at timestamp with time zone,
    catalog_key_id text,
    CONSTRAINT pricing_source_snapshots_catalog CHECK ((num_nulls(catalog_sha256, catalog_published_at, catalog_key_id) = ANY (ARRAY[0, 3]))),
    CONSTRAINT pricing_source_snapshots_catalog_sha256_check CHECK ((catalog_sha256 ~ '^[0-9a-f]{64}$'::text)),
    CONSTRAINT pricing_source_snapshots_document_check CHECK ((octet_length((document)::text) <= 4194304)),
    CONSTRAINT pricing_source_snapshots_sha256_check CHECK ((sha256 ~ '^[0-9a-f]{64}$'::text))
);

CREATE TABLE olp.pricing_sources (
    id uuid NOT NULL,
    name text NOT NULL,
    url text,
    enabled boolean DEFAULT true NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    format text DEFAULT 'prices'::text NOT NULL,
    catalog_published_at timestamp with time zone,
    CONSTRAINT pricing_sources_format_check CHECK ((format = ANY (ARRAY['prices'::text, 'catalog'::text]))),
    CONSTRAINT pricing_sources_url_format CHECK (((url IS NOT NULL) OR (format = 'catalog'::text)))
);

CREATE TABLE olp.prompt_labels (
    publication_id uuid NOT NULL,
    label text NOT NULL,
    revision_id uuid NOT NULL,
    etag uuid NOT NULL,
    CONSTRAINT prompt_labels_label_check CHECK ((label = ANY (ARRAY['production'::text, 'staging'::text])))
);

CREATE TABLE olp.provider_billing_contexts (
    provider_id uuid NOT NULL,
    source text NOT NULL,
    scope jsonb NOT NULL,
    imported_from date,
    imported_through date,
    last_success_at timestamp with time zone
);

CREATE TABLE olp.provider_billing_days (
    provider_id uuid NOT NULL,
    day date NOT NULL,
    source text NOT NULL,
    scope jsonb,
    last_success_at timestamp with time zone
);

CREATE TABLE olp.provider_billing_imports (
    provider_id uuid NOT NULL,
    source text NOT NULL,
    credential_id uuid,
    scope jsonb DEFAULT '{}'::jsonb NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    next_import_at timestamp with time zone DEFAULT now() NOT NULL,
    last_import_at timestamp with time zone,
    last_success_at timestamp with time zone,
    last_error_code text,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    imported_from date,
    imported_through date,
    source_revision text,
    resume_period date,
    resume_remaining integer DEFAULT 0 NOT NULL,
    CONSTRAINT provider_billing_imports_resume_check CHECK ((((resume_period IS NULL) AND (resume_remaining = 0)) OR ((resume_period IS NOT NULL) AND (resume_remaining > 0)))),
    CONSTRAINT provider_billing_imports_scope_check CHECK (((jsonb_typeof(scope) = 'object'::text) AND (octet_length((scope)::text) <= 65536))),
    CONSTRAINT provider_billing_imports_source_check CHECK ((source = ANY (ARRAY['openai_costs'::text, 'anthropic_cost_report'::text, 'aws_cur'::text, 'google_bigquery'::text, 'azure_cost_management'::text])))
);

CREATE TABLE olp.provider_billing_revisions (
    provider_id uuid NOT NULL,
    source text NOT NULL,
    scope_digest bytea NOT NULL,
    period_start date NOT NULL,
    source_revision text NOT NULL,
    ordering_at timestamp with time zone NOT NULL,
    covered_from date,
    covered_through date
);

CREATE TABLE olp.provider_credentials (
    id uuid NOT NULL,
    provider_id uuid NOT NULL,
    version integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    plugin_digest text,
    principal text,
    grant_facts jsonb,
    profile_id text,
    external_reference jsonb,
    CONSTRAINT provider_credential_reference_shape CHECK (((external_reference IS NULL) OR ((jsonb_typeof(external_reference) = 'object'::text) AND (octet_length((external_reference)::text) <= 8192) AND (plugin_digest IS NULL)))),
    CONSTRAINT provider_credentials_check CHECK (((profile_id IS NULL) OR ((plugin_digest IS NOT NULL) AND (profile_id <> ''::text)))),
    CONSTRAINT provider_credentials_grant_check CHECK ((((plugin_digest IS NULL) = (principal IS NULL)) AND ((plugin_digest IS NULL) = (grant_facts IS NULL))))
);

CREATE TABLE olp.provider_grants (
    credential_id uuid NOT NULL,
    refresh_token_id uuid,
    expires_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    generation bigint DEFAULT 1 NOT NULL,
    refresh_at timestamp with time zone,
    refresh_failures integer DEFAULT 0 NOT NULL,
    refresh_failure text,
    lapsed_at timestamp with time zone,
    refresh_attempt_id uuid,
    CONSTRAINT provider_grants_lapsed_check CHECK (((lapsed_at IS NULL) OR ((refresh_token_id IS NULL) AND (refresh_at IS NULL)))),
    CONSTRAINT provider_grants_refresh_attempt_check CHECK (((refresh_attempt_id IS NULL) OR ((refresh_token_id IS NOT NULL) AND (refresh_at IS NOT NULL)))),
    CONSTRAINT provider_grants_refresh_failures_check CHECK ((refresh_failures >= 0))
);

CREATE TABLE olp.provider_invoiced_costs (
    provider_id uuid NOT NULL,
    day date NOT NULL,
    line_item text NOT NULL,
    currency character(3) NOT NULL,
    amount numeric NOT NULL,
    source text NOT NULL,
    fetched_at timestamp with time zone DEFAULT now() NOT NULL,
    source_revision text,
    CONSTRAINT provider_invoiced_costs_currency_check CHECK ((((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text))),
    CONSTRAINT provider_invoiced_costs_line_item_check CHECK (((length(line_item) >= 1) AND (length(line_item) <= 256)))
);

CREATE TABLE olp.provider_models (
    id uuid NOT NULL,
    provider_id uuid NOT NULL,
    upstream_model text NOT NULL,
    display_name text NOT NULL,
    enabled boolean DEFAULT false NOT NULL,
    capabilities jsonb DEFAULT '[]'::jsonb NOT NULL,
    discovered_at timestamp with time zone,
    discovered_listing_endpoint text,
    discovered_listing_api_version text,
    discovered_listing_credential_id uuid,
    discovered_listing_credential_version integer,
    discovered_listing_at timestamp with time zone,
    CONSTRAINT provider_models_listing_provenance_check CHECK ((num_nonnulls(discovered_listing_endpoint, discovered_listing_api_version, discovered_listing_credential_id, discovered_listing_credential_version, discovered_listing_at) = ANY (ARRAY[0, 5])))
);

CREATE TABLE olp.provider_network_credentials (
    id uuid NOT NULL,
    provider_id uuid NOT NULL,
    version integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone
);

CREATE TABLE olp.provider_revisions (
    id uuid NOT NULL,
    provider_id uuid NOT NULL,
    revision integer NOT NULL,
    name text NOT NULL,
    configuration json NOT NULL,
    models jsonb NOT NULL,
    slots jsonb NOT NULL,
    credential_version integer,
    source_etag uuid NOT NULL,
    activated_by uuid NOT NULL,
    activated_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON COLUMN olp.provider_revisions.configuration IS 'Immutable authoritative provider configuration at activation; never normalize through jsonb.';

CREATE TABLE olp.provider_slots (
    id uuid NOT NULL,
    provider_id uuid NOT NULL,
    is_default boolean DEFAULT false NOT NULL,
    "position" integer NOT NULL,
    name text NOT NULL,
    enabled boolean DEFAULT true NOT NULL,
    priority integer DEFAULT 0 NOT NULL,
    weight integer DEFAULT 1 NOT NULL,
    credential_id uuid,
    restrictions jsonb DEFAULT '{}'::jsonb NOT NULL,
    limits jsonb DEFAULT '{}'::jsonb NOT NULL,
    validated_at timestamp with time zone,
    validated_fingerprint text,
    etag uuid DEFAULT gen_random_uuid() NOT NULL
);

CREATE TABLE olp.providers (
    id uuid NOT NULL,
    name text NOT NULL,
    kind text NOT NULL,
    state text NOT NULL,
    configuration json NOT NULL,
    etag uuid NOT NULL,
    slots_etag uuid NOT NULL,
    draft_dirty boolean DEFAULT true NOT NULL,
    active_revision integer,
    active_revision_id uuid,
    last_probe_at timestamp with time zone,
    last_probe_status text,
    last_probe_detail text,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    project_id uuid,
    CONSTRAINT providers_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'active'::text, 'disabled'::text])))
);

COMMENT ON COLUMN olp.providers.configuration IS 'Authoritative provider configuration; native JSON number spellings and subtrees are preserved.';

CREATE TABLE olp.provisioned_users (
    source text NOT NULL,
    external_id text NOT NULL,
    user_id uuid NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT provisioned_users_check CHECK (((source <> ''::text) AND (octet_length(source) <= 100) AND (external_id <> ''::text) AND (octet_length(external_id) <= 255)))
);

CREATE TABLE olp.rate_card_prices (
    rate_card_revision_id uuid NOT NULL,
    provider_kind text NOT NULL,
    vendor_id text,
    model text NOT NULL,
    operation text NOT NULL,
    input_per_million numeric(24,12),
    cached_input_per_million numeric(24,12),
    cache_write_input_per_million numeric(24,12),
    cache_write_5m_input_per_million numeric(24,12),
    cache_write_1h_input_per_million numeric(24,12),
    output_per_million numeric(24,12),
    audio_input_per_million numeric(24,12),
    cached_audio_input_per_million numeric(24,12),
    audio_output_per_million numeric(24,12),
    image_input_per_million numeric(24,12),
    cached_image_input_per_million numeric(24,12),
    image_output_per_million numeric(24,12),
    unit_price numeric(24,12),
    per_request numeric(24,12),
    batch_multiplier numeric(24,12),
    tool_calls jsonb,
    tiers jsonb,
    windows jsonb,
    currency character(3) NOT NULL,
    training_per_million numeric(24,12),
    storage_per_gib_day numeric(24,12),
    cache_storage_per_million_token_hour numeric(24,12),
    container_per_session jsonb,
    CONSTRAINT rate_card_prices_cache_storage_per_million_token_hour_check CHECK ((cache_storage_per_million_token_hour >= (0)::numeric)),
    CONSTRAINT rate_card_prices_currency_check CHECK ((((currency)::text = upper((currency)::text)) AND (btrim((currency)::text) ~ '^[A-Z]{3}$'::text))),
    CONSTRAINT rate_card_prices_operation_check CHECK ((operation ~ '^[a-z][a-z0-9_-]{0,95}$'::text)),
    CONSTRAINT rate_card_prices_provider_kind_check CHECK ((provider_kind = ANY (ARRAY['openai'::text, 'anthropic'::text, 'gemini'::text, 'vertex_ai'::text, 'bedrock'::text, 'sagemaker'::text, 'watsonx'::text, 'azure_openai'::text, 'azure_ai_search'::text, 'openai_compatible'::text, 'plugin'::text]))),
    CONSTRAINT rate_card_prices_storage_per_gib_day_check CHECK ((storage_per_gib_day >= (0)::numeric)),
    CONSTRAINT rate_card_prices_tiers_check CHECK (((tiers IS NULL) OR ((jsonb_typeof(tiers) = 'array'::text) AND (pg_column_size(tiers) <= 65536)))),
    CONSTRAINT rate_card_prices_tool_calls_check CHECK (((tool_calls IS NULL) OR ((jsonb_typeof(tool_calls) = 'object'::text) AND (pg_column_size(tool_calls) <= 8192)))),
    CONSTRAINT rate_card_prices_training_per_million_check CHECK ((training_per_million >= (0)::numeric)),
    CONSTRAINT rate_card_prices_windows_check CHECK (((windows IS NULL) OR ((jsonb_typeof(windows) = 'object'::text) AND (pg_column_size(windows) <= 65536)))),
    CONSTRAINT retained_card_container_rates_object CHECK (((container_per_session IS NULL) OR (jsonb_typeof(container_per_session) = 'object'::text)))
);

CREATE TABLE olp.rate_card_revisions (
    id uuid NOT NULL,
    rate_card_id uuid NOT NULL,
    revision integer NOT NULL,
    markup numeric(24,12) NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by uuid NOT NULL
);

CREATE TABLE olp.rate_cards (
    id uuid NOT NULL,
    organization_id uuid,
    project_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    created_by uuid NOT NULL,
    CONSTRAINT rate_cards_owner CHECK (((project_id IS NULL) OR (organization_id IS NULL)))
);

CREATE TABLE olp.recent_auth (
    digest bytea NOT NULL,
    session_id uuid NOT NULL,
    purpose text NOT NULL,
    resource_id uuid,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT recent_auth_purpose_check CHECK ((purpose = ANY (ARRAY['password_enrollment'::text, 'oidc_link'::text, 'oidc_unlink'::text, 'saml_link'::text, 'saml_unlink'::text, 'plugin_permit'::text, 'mfa_manage'::text, 'agent_delegation'::text])))
);

CREATE TABLE olp.regional_worker_task_health (
    task text NOT NULL,
    checked_at timestamp with time zone NOT NULL,
    last_success_at timestamp with time zone,
    last_progress_at timestamp with time zone,
    successes_total bigint DEFAULT 0 NOT NULL,
    failures_total bigint DEFAULT 0 NOT NULL,
    skipped_total bigint DEFAULT 0 NOT NULL,
    first_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    region text NOT NULL,
    CONSTRAINT regional_worker_task_health_region_check CHECK (((length(region) >= 1) AND (length(region) <= 100))),
    CONSTRAINT worker_task_health_failures_total_check CHECK ((failures_total >= 0)),
    CONSTRAINT worker_task_health_skipped_total_check CHECK ((skipped_total >= 0)),
    CONSTRAINT worker_task_health_successes_total_check CHECK ((successes_total >= 0)),
    CONSTRAINT worker_task_health_task_check CHECK ((task = ANY (ARRAY['request_metadata_consumer'::text, 'maintenance'::text, 'cost_reconciliation'::text, 'request_metadata_gateway_epoch_detection'::text, 'media_reconciliation'::text, 'notification_delivery'::text, 'grant_refresh'::text, 'health_probes'::text, 'export_delivery'::text])))
);

CREATE TABLE olp.replays (
    actor uuid NOT NULL,
    key text NOT NULL,
    fingerprint bytea NOT NULL,
    secret_id uuid,
    expires_at timestamp with time zone NOT NULL
);

CREATE TABLE olp.request_metadata_consumer_health (
    singleton boolean DEFAULT true NOT NULL,
    pending_events bigint NOT NULL,
    lag_events bigint NOT NULL,
    oldest_pending_at timestamp with time zone,
    checked_at timestamp with time zone NOT NULL,
    oldest_lagged_at timestamp with time zone,
    CONSTRAINT request_metadata_consumer_health_lag_events_check CHECK ((lag_events >= 0)),
    CONSTRAINT request_metadata_consumer_health_pending_events_check CHECK ((pending_events >= 0)),
    CONSTRAINT request_metadata_consumer_health_singleton_check CHECK (singleton)
);

CREATE TABLE olp.request_metadata_event_receipts (
    event_id uuid NOT NULL,
    request_id uuid NOT NULL,
    event_sha256 bytea NOT NULL,
    status text NOT NULL,
    observed_at timestamp with time zone NOT NULL,
    recorded_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT request_metadata_event_receipts_event_sha256_check CHECK ((octet_length(event_sha256) = 32)),
    CONSTRAINT request_metadata_event_receipts_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'fact_persisted'::text, 'rejected'::text])))
);

CREATE TABLE olp.request_metadata_gap_hourly (
    bucket timestamp with time zone NOT NULL,
    gateway_instance text NOT NULL,
    reason text NOT NULL,
    event_count bigint NOT NULL,
    first_observed_at timestamp with time zone NOT NULL,
    last_observed_at timestamp with time zone NOT NULL,
    uncertain_gap_count bigint DEFAULT 0 NOT NULL,
    CONSTRAINT request_metadata_gap_hourly_bucket_utc_check CHECK ((bucket = (date_trunc('hour'::text, (first_observed_at AT TIME ZONE 'UTC'::text)) AT TIME ZONE 'UTC'::text))),
    CONSTRAINT request_metadata_gap_hourly_event_count_check CHECK ((event_count >= 0)),
    CONSTRAINT request_metadata_gap_hourly_evidence_check CHECK (((event_count > 0) OR (uncertain_gap_count > 0))),
    CONSTRAINT request_metadata_gap_hourly_uncertain_gap_count_check CHECK ((uncertain_gap_count >= 0)),
    CONSTRAINT request_metadata_gap_hourly_window_check CHECK ((last_observed_at >= first_observed_at))
);

CREATE TABLE olp.request_metadata_gateway_epochs (
    gateway_instance text NOT NULL,
    process_epoch uuid NOT NULL,
    started_at timestamp with time zone NOT NULL,
    accepted bigint NOT NULL,
    persisted bigint NOT NULL,
    dropped bigint NOT NULL,
    abandoned bigint NOT NULL,
    retrying boolean NOT NULL,
    writer_closed boolean NOT NULL,
    updated_at timestamp with time zone NOT NULL,
    gracefully_closed_at timestamp with time zone,
    stale_candidate_at timestamp with time zone,
    stale_detected_at timestamp with time zone,
    acknowledged_at timestamp with time zone,
    acknowledged_by uuid,
    uncertainty_gap_id uuid,
    CONSTRAINT request_metadata_gateway_epochs_abandoned_check CHECK ((abandoned >= 0)),
    CONSTRAINT request_metadata_gateway_epochs_accepted_check CHECK ((accepted >= 0)),
    CONSTRAINT request_metadata_gateway_epochs_acknowledged_check CHECK (((acknowledged_at IS NULL) OR ((stale_detected_at IS NOT NULL) AND (acknowledged_at >= stale_detected_at)))),
    CONSTRAINT request_metadata_gateway_epochs_candidate_check CHECK (((stale_candidate_at IS NULL) OR (stale_candidate_at >= started_at))),
    CONSTRAINT request_metadata_gateway_epochs_closed_check CHECK (((gracefully_closed_at IS NULL) OR (gracefully_closed_at >= started_at))),
    CONSTRAINT request_metadata_gateway_epochs_detected_check CHECK (((stale_detected_at IS NULL) OR (stale_detected_at >= started_at))),
    CONSTRAINT request_metadata_gateway_epochs_dropped_check CHECK ((dropped >= 0)),
    CONSTRAINT request_metadata_gateway_epochs_persisted_check CHECK ((persisted >= 0)),
    CONSTRAINT request_metadata_gateway_epochs_resolution_check CHECK ((NOT ((gracefully_closed_at IS NOT NULL) AND (stale_detected_at IS NOT NULL)))),
    CONSTRAINT request_metadata_gateway_epochs_updated_check CHECK ((updated_at >= started_at))
);

CREATE TABLE olp.request_metadata_ingestion_gaps (
    id uuid NOT NULL,
    gateway_instance text NOT NULL,
    event_count bigint NOT NULL,
    reason text NOT NULL,
    first_observed_at timestamp with time zone NOT NULL,
    last_observed_at timestamp with time zone NOT NULL,
    reported_at timestamp with time zone DEFAULT now() NOT NULL,
    certainty text DEFAULT 'exact'::text NOT NULL,
    deduplication_key text,
    CONSTRAINT request_metadata_ingestion_gaps_certainty_check CHECK ((certainty = ANY (ARRAY['exact'::text, 'lower_bound'::text]))),
    CONSTRAINT request_metadata_ingestion_gaps_count_check CHECK (((certainty = 'lower_bound'::text) OR (event_count > 0))),
    CONSTRAINT request_metadata_ingestion_gaps_deduplication_key_check CHECK (((deduplication_key IS NULL) OR ((deduplication_key <> ''::text) AND (octet_length(deduplication_key) <= 256)))),
    CONSTRAINT request_metadata_ingestion_gaps_event_count_check CHECK ((event_count >= 0)),
    CONSTRAINT request_metadata_ingestion_gaps_window_check CHECK ((last_observed_at >= first_observed_at))
);

CREATE TABLE olp.requests (
    id uuid NOT NULL,
    runtime_generation_id uuid NOT NULL,
    api_key_id uuid,
    route_slug text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    status_code integer,
    error_class text,
    total_latency_ms integer,
    first_byte_ms integer,
    attempt_count smallint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    budget_group_id uuid,
    attribution jsonb DEFAULT '{}'::jsonb NOT NULL,
    origin text DEFAULT 'caller'::text NOT NULL,
    parent_request_id uuid,
    end_user_digest text DEFAULT ''::text NOT NULL,
    budget_boundary text DEFAULT ''::text NOT NULL,
    payload_captured boolean DEFAULT false NOT NULL,
    guardrail_decisions jsonb DEFAULT '[]'::jsonb NOT NULL,
    evaluator_guardrail_id uuid,
    cache_result text,
    cache_lookup_reason text,
    CONSTRAINT requests_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT requests_attribution_check CHECK (((jsonb_typeof(attribution) = 'object'::text) AND (octet_length((attribution)::text) <= 1024))),
    CONSTRAINT requests_budget_boundary_check CHECK ((budget_boundary = ANY (ARRAY[''::text, 'api_key'::text, 'budget_group'::text, 'key_route'::text, 'key_end_user'::text, 'project_end_user'::text, 'attribution'::text, 'project'::text, 'organization'::text, 'installation'::text]))),
    CONSTRAINT requests_cache_result_check CHECK ((cache_result = ANY (ARRAY['hit'::text, 'semantic-hit'::text, 'miss'::text, 'bypass'::text]))),
    CONSTRAINT requests_check CHECK (((cache_lookup_reason IS NULL) OR ((cache_lookup_reason = 'refresh'::text) AND (NOT (cache_result IS DISTINCT FROM 'miss'::text))) OR ((cache_lookup_reason = ANY (ARRAY['no_store'::text, 'multiple_choices'::text, 'backend_unavailable'::text])) AND (NOT (cache_result IS DISTINCT FROM 'bypass'::text))))),
    CONSTRAINT requests_end_user_digest_check CHECK (((end_user_digest = ''::text) OR (end_user_digest ~ '^[0-9a-f]{64}$'::text))),
    CONSTRAINT requests_first_byte_ms_check CHECK (((first_byte_ms IS NULL) OR (first_byte_ms >= 0))),
    CONSTRAINT requests_guardrail_decisions_check CHECK (((jsonb_typeof(guardrail_decisions) = 'array'::text) AND (octet_length((guardrail_decisions)::text) <= 262144))),
    CONSTRAINT requests_origin_check CHECK ((origin = ANY (ARRAY['caller'::text, 'shadow'::text, 'classifier'::text, 'probe'::text, 'playground'::text, 'cache_embedding'::text, 'batch_settlement'::text, 'retained_settlement'::text]))),
    CONSTRAINT requests_origin_key_check CHECK (((api_key_id IS NULL) = (origin = ANY (ARRAY['shadow'::text, 'probe'::text, 'playground'::text])))),
    CONSTRAINT requests_origin_parent_check CHECK (((parent_request_id IS NOT NULL) = ((origin = ANY (ARRAY['shadow'::text, 'classifier'::text, 'cache_embedding'::text, 'batch_settlement'::text, 'retained_settlement'::text])) OR (evaluator_guardrail_id IS NOT NULL)))),
    CONSTRAINT requests_status_code_check CHECK (((status_code IS NULL) OR ((status_code >= 100) AND (status_code <= 599)))),
    CONSTRAINT requests_total_latency_ms_check CHECK (((total_latency_ms IS NULL) OR (total_latency_ms >= 0)))
)
PARTITION BY RANGE (started_at);

CREATE TABLE olp.requests_default (
    id uuid NOT NULL,
    runtime_generation_id uuid NOT NULL,
    api_key_id uuid,
    route_slug text NOT NULL,
    operation text NOT NULL,
    surface text NOT NULL,
    started_at timestamp with time zone NOT NULL,
    completed_at timestamp with time zone,
    status_code integer,
    error_class text,
    total_latency_ms integer,
    first_byte_ms integer,
    attempt_count smallint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    budget_group_id uuid,
    attribution jsonb DEFAULT '{}'::jsonb NOT NULL,
    origin text DEFAULT 'caller'::text NOT NULL,
    parent_request_id uuid,
    end_user_digest text DEFAULT ''::text NOT NULL,
    budget_boundary text DEFAULT ''::text NOT NULL,
    payload_captured boolean DEFAULT false NOT NULL,
    guardrail_decisions jsonb DEFAULT '[]'::jsonb NOT NULL,
    evaluator_guardrail_id uuid,
    cache_result text,
    cache_lookup_reason text,
    CONSTRAINT requests_attempt_count_check CHECK ((attempt_count >= 0)),
    CONSTRAINT requests_attribution_check CHECK (((jsonb_typeof(attribution) = 'object'::text) AND (octet_length((attribution)::text) <= 1024))),
    CONSTRAINT requests_budget_boundary_check CHECK ((budget_boundary = ANY (ARRAY[''::text, 'api_key'::text, 'budget_group'::text, 'key_route'::text, 'key_end_user'::text, 'project_end_user'::text, 'attribution'::text, 'project'::text, 'organization'::text, 'installation'::text]))),
    CONSTRAINT requests_cache_result_check CHECK ((cache_result = ANY (ARRAY['hit'::text, 'semantic-hit'::text, 'miss'::text, 'bypass'::text]))),
    CONSTRAINT requests_check CHECK (((cache_lookup_reason IS NULL) OR ((cache_lookup_reason = 'refresh'::text) AND (NOT (cache_result IS DISTINCT FROM 'miss'::text))) OR ((cache_lookup_reason = ANY (ARRAY['no_store'::text, 'multiple_choices'::text, 'backend_unavailable'::text])) AND (NOT (cache_result IS DISTINCT FROM 'bypass'::text))))),
    CONSTRAINT requests_end_user_digest_check CHECK (((end_user_digest = ''::text) OR (end_user_digest ~ '^[0-9a-f]{64}$'::text))),
    CONSTRAINT requests_first_byte_ms_check CHECK (((first_byte_ms IS NULL) OR (first_byte_ms >= 0))),
    CONSTRAINT requests_guardrail_decisions_check CHECK (((jsonb_typeof(guardrail_decisions) = 'array'::text) AND (octet_length((guardrail_decisions)::text) <= 262144))),
    CONSTRAINT requests_origin_check CHECK ((origin = ANY (ARRAY['caller'::text, 'shadow'::text, 'classifier'::text, 'probe'::text, 'playground'::text, 'cache_embedding'::text, 'batch_settlement'::text, 'retained_settlement'::text]))),
    CONSTRAINT requests_origin_key_check CHECK (((api_key_id IS NULL) = (origin = ANY (ARRAY['shadow'::text, 'probe'::text, 'playground'::text])))),
    CONSTRAINT requests_origin_parent_check CHECK (((parent_request_id IS NOT NULL) = ((origin = ANY (ARRAY['shadow'::text, 'classifier'::text, 'cache_embedding'::text, 'batch_settlement'::text, 'retained_settlement'::text])) OR (evaluator_guardrail_id IS NOT NULL)))),
    CONSTRAINT requests_status_code_check CHECK (((status_code IS NULL) OR ((status_code >= 100) AND (status_code <= 599)))),
    CONSTRAINT requests_total_latency_ms_check CHECK (((total_latency_ms IS NULL) OR (total_latency_ms >= 0)))
);

CREATE TABLE olp.retained_mutations (
    api_key_id uuid NOT NULL,
    operation text NOT NULL,
    submission_key text NOT NULL,
    resource_id uuid NOT NULL,
    request_digest bytea NOT NULL,
    state text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT retained_mutations_request_digest_check CHECK ((octet_length(request_digest) = 32)),
    CONSTRAINT retained_mutations_state_check CHECK ((state = ANY (ARRAY['prepared'::text, 'dispatched'::text, 'accepted'::text, 'submission_unknown'::text, 'not_accepted'::text]))),
    CONSTRAINT retained_mutations_submission_key_check CHECK (((octet_length(submission_key) >= 1) AND (octet_length(submission_key) <= 128)))
);

CREATE TABLE olp.retained_submissions (
    resource_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    operation text NOT NULL,
    submission_key text NOT NULL,
    request_digest bytea NOT NULL,
    state text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT retained_submissions_request_digest_check CHECK ((octet_length(request_digest) = 32)),
    CONSTRAINT retained_submissions_state_check CHECK ((state = ANY (ARRAY['prepared'::text, 'dispatched'::text, 'accepted'::text, 'submission_unknown'::text, 'not_accepted'::text]))),
    CONSTRAINT retained_submissions_submission_key_check CHECK (((octet_length(submission_key) >= 1) AND (octet_length(submission_key) <= 128)))
);

COMMENT ON TABLE olp.retained_submissions IS 'At-most-once creation fences. Unknown acceptance never authorizes redispatch or expires with resource retention.';

CREATE TABLE olp.retained_usage_coverage (
    resource_id uuid NOT NULL,
    evidence_id uuid NOT NULL,
    component text NOT NULL,
    model text NOT NULL,
    memory_tier text DEFAULT ''::text NOT NULL,
    starts_at timestamp with time zone NOT NULL,
    ends_at timestamp with time zone NOT NULL,
    quantities jsonb NOT NULL,
    event_id uuid NOT NULL,
    CONSTRAINT retained_usage_coverage_check CHECK ((ends_at > starts_at)),
    CONSTRAINT retained_usage_coverage_component_check CHECK ((component = ANY (ARRAY['training'::text, 'storage'::text, 'cache_storage'::text, 'container'::text, 'generation'::text])))
);

CREATE TABLE olp.retained_usage_evidence (
    id uuid NOT NULL,
    resource_id uuid NOT NULL,
    idempotency_key text NOT NULL,
    digest text NOT NULL,
    provenance text NOT NULL,
    actor text,
    document jsonb NOT NULL,
    remote_termination_verified boolean NOT NULL,
    verified_terminal_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT retained_usage_evidence_digest_check CHECK ((digest ~ '^[a-f0-9]{64}$'::text)),
    CONSTRAINT retained_usage_evidence_idempotency_key_check CHECK (((octet_length(idempotency_key) >= 1) AND (octet_length(idempotency_key) <= 128))),
    CONSTRAINT retained_usage_evidence_provenance_check CHECK ((provenance = ANY (ARRAY['provider_verified'::text, 'operator_verified'::text])))
);

CREATE TABLE olp.retrieval_binding_revisions (
    id uuid NOT NULL,
    binding_id uuid NOT NULL,
    revision bigint NOT NULL,
    definition jsonb NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT retrieval_binding_revisions_definition_check CHECK ((jsonb_typeof(definition) = 'object'::text)),
    CONSTRAINT retrieval_binding_revisions_revision_check CHECK ((revision > 0))
);

CREATE TABLE olp.retrieval_bindings (
    id uuid NOT NULL,
    project_id uuid NOT NULL,
    name text NOT NULL,
    latest_revision_id uuid NOT NULL,
    etag uuid NOT NULL,
    retired_at timestamp with time zone,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT retrieval_bindings_name_check CHECK (((octet_length(name) >= 1) AND (octet_length(name) <= 100)))
);

CREATE TABLE olp.route_drafts (
    id uuid NOT NULL,
    slug text NOT NULL,
    state text NOT NULL,
    operations jsonb NOT NULL,
    overall_timeout_ms integer NOT NULL,
    max_attempts integer NOT NULL,
    targets jsonb NOT NULL,
    based_on_revision_id uuid,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    project_id uuid,
    fidelity jsonb NOT NULL,
    behavior jsonb DEFAULT '{}'::jsonb NOT NULL,
    catalog_expose_upstream_models boolean,
    CONSTRAINT route_drafts_behavior_check CHECK ((jsonb_typeof(behavior) = 'object'::text)),
    CONSTRAINT route_drafts_fidelity_check CHECK ((fidelity = ANY (ARRAY['{"mode": "strict"}'::jsonb, '{"mode": "transformed"}'::jsonb]))),
    CONSTRAINT route_drafts_state_check CHECK ((state = ANY (ARRAY['draft'::text, 'validated'::text])))
);

CREATE TABLE olp.route_revisions (
    id uuid NOT NULL,
    route_id uuid NOT NULL,
    revision integer NOT NULL,
    slug text NOT NULL,
    operations jsonb NOT NULL,
    overall_timeout_ms integer NOT NULL,
    max_attempts integer NOT NULL,
    targets jsonb NOT NULL,
    source_draft_id uuid NOT NULL,
    activated_by uuid NOT NULL,
    activated_at timestamp with time zone DEFAULT now() NOT NULL,
    routing_policy jsonb NOT NULL,
    fidelity jsonb NOT NULL,
    behavior jsonb DEFAULT '{}'::jsonb NOT NULL,
    CONSTRAINT route_revisions_behavior_check CHECK ((jsonb_typeof(behavior) = 'object'::text)),
    CONSTRAINT route_revisions_fidelity_check CHECK ((fidelity = ANY (ARRAY['{"mode": "strict"}'::jsonb, '{"mode": "transformed"}'::jsonb])))
);

CREATE TABLE olp.route_template_routes (
    template_id uuid NOT NULL,
    provider_model_id uuid NOT NULL,
    provider_id uuid NOT NULL,
    upstream_model text NOT NULL,
    route_slug text NOT NULL,
    draft_id uuid,
    outcome text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT route_template_routes_outcome_check CHECK ((outcome = ANY (ARRAY['draft'::text, 'published'::text])))
);

CREATE TABLE olp.route_templates (
    id uuid NOT NULL,
    name text NOT NULL,
    provider_selector text NOT NULL,
    model_filter text NOT NULL,
    slug_pattern text NOT NULL,
    overall_timeout_ms integer NOT NULL,
    max_attempts integer NOT NULL,
    fidelity jsonb NOT NULL,
    routing_policy jsonb,
    auto_publish boolean DEFAULT false NOT NULL,
    project_id uuid,
    etag uuid NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT route_templates_fidelity_check CHECK ((fidelity = ANY (ARRAY['{"mode": "strict"}'::jsonb, '{"mode": "transformed"}'::jsonb]))),
    CONSTRAINT route_templates_name_check CHECK ((name ~ '^[a-z0-9][a-z0-9-]{0,62}$'::text))
);

CREATE TABLE olp.routes (
    id uuid NOT NULL,
    slug text NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    latest_revision integer NOT NULL,
    latest_revision_id uuid NOT NULL,
    state text DEFAULT 'active'::text NOT NULL,
    etag uuid NOT NULL,
    retired_at timestamp with time zone,
    retired_by uuid,
    project_id uuid,
    CONSTRAINT routes_retirement_check CHECK ((((state = 'active'::text) AND (retired_at IS NULL) AND (retired_by IS NULL)) OR ((state = 'retired'::text) AND (retired_at IS NOT NULL) AND (retired_by IS NOT NULL)))),
    CONSTRAINT routes_state_check CHECK ((state = ANY (ARRAY['active'::text, 'retired'::text])))
);

CREATE TABLE olp.routing_policies (
    scope text NOT NULL,
    scope_id uuid NOT NULL,
    policy jsonb NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    configured boolean DEFAULT true NOT NULL,
    CONSTRAINT routing_policies_scope_check CHECK ((scope = ANY (ARRAY['installation'::text, 'route-draft'::text, 'api-key'::text]))),
    CONSTRAINT routing_policy_tombstone_default CHECK ((configured OR (policy = '{}'::jsonb)))
);

CREATE TABLE olp.runtime_install_status (
    gateway_instance text NOT NULL,
    desired_generation bigint NOT NULL,
    installed_generation bigint NOT NULL,
    failed boolean NOT NULL,
    checked_at timestamp with time zone DEFAULT now() NOT NULL,
    authority_sequence bigint DEFAULT 0 NOT NULL,
    CONSTRAINT runtime_install_status_gateway_instance_check CHECK (((length(gateway_instance) >= 1) AND (length(gateway_instance) <= 256)))
);

CREATE TABLE olp.runtime_releases (
    id uuid NOT NULL,
    sequence bigint NOT NULL,
    sha256 text NOT NULL,
    snapshot json NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    published_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON COLUMN olp.runtime_releases.snapshot IS 'Authoritative serving snapshot. Preserve native JSON source and its recorded sha256; never rewrite a digest after normalization.';

CREATE TABLE olp.runtime_shared_quota_retirements (
    sequence bigint NOT NULL,
    project_id text NOT NULL,
    group_name text NOT NULL
);

CREATE TABLE olp.saml_assertion_replays (
    digest bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    CONSTRAINT saml_assertion_replays_digest_check CHECK ((octet_length(digest) = 32))
);

CREATE TABLE olp.saml_configuration (
    singleton boolean DEFAULT true NOT NULL,
    id uuid NOT NULL,
    document jsonb NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT saml_configuration_document_check CHECK ((jsonb_typeof(document) = 'object'::text)),
    CONSTRAINT saml_configuration_singleton_check CHECK (singleton)
);

CREATE TABLE olp.saml_flows (
    id uuid NOT NULL,
    state_digest bytea NOT NULL,
    cookie_digest bytea NOT NULL,
    configuration_etag uuid NOT NULL,
    request_id text NOT NULL,
    received_at timestamp with time zone,
    expires_at timestamp with time zone NOT NULL
);

CREATE TABLE olp.saml_identities (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    issuer text NOT NULL,
    subject text NOT NULL,
    email_at_link text NOT NULL,
    role_claims jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_login_at timestamp with time zone
);

CREATE TABLE olp.scim_groups (
    id uuid NOT NULL,
    document jsonb NOT NULL,
    etag uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT scim_groups_document_check CHECK ((jsonb_typeof(document) = 'object'::text))
);

CREATE TABLE olp.secrets (
    id uuid NOT NULL,
    purpose text NOT NULL,
    key_version integer NOT NULL,
    ciphertext bytea NOT NULL,
    expires_at timestamp with time zone,
    CONSTRAINT secrets_continuation_ciphertext_bound CHECK (((purpose <> 'provider_continuation'::text) OR ((expires_at IS NOT NULL) AND (octet_length(ciphertext) <= 5242880)))),
    CONSTRAINT secrets_grant_enrollment_bound CHECK (((purpose <> 'grant_enrollment'::text) OR ((expires_at IS NOT NULL) AND (octet_length(ciphertext) <= 65536)))),
    CONSTRAINT secrets_media_job_source_bound CHECK (((purpose <> 'media_job_source'::text) OR ((expires_at IS NOT NULL) AND (octet_length(ciphertext) <= 1048604)))),
    CONSTRAINT secrets_purpose_check CHECK ((purpose = ANY (ARRAY['provider_credential'::text, 'provider_continuation'::text, 'mfa_totp'::text, 'mfa_webauthn'::text, 'notification_secret'::text, 'mutation_replay'::text, 'oidc_client'::text, 'oidc_flow'::text, 'media_job_source'::text, 'provider_grant_refresh'::text, 'grant_enrollment'::text, 'saml_key'::text, 'saml_flow'::text, 'sink_credential'::text, 'mcp_credential'::text, 'billing_credential'::text, 'guardrail_credential'::text, 'provider_upload_session'::text, 'response_cache'::text, 'agent_credential'::text, 'mcp_grant_access'::text, 'mcp_grant_refresh'::text, 'mcp_grant_enrollment'::text, 'mcp_delegation_signing'::text, 'mcp_entra_client'::text])))
);

CREATE TABLE olp.sessions (
    id uuid NOT NULL,
    user_id uuid NOT NULL,
    digest bytea NOT NULL,
    expires_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    last_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    browser_hint text NOT NULL,
    auth_method text DEFAULT 'local'::text NOT NULL,
    mfa_verified boolean DEFAULT false NOT NULL,
    CONSTRAINT sessions_auth_method_check CHECK ((auth_method = ANY (ARRAY['local'::text, 'oidc'::text, 'saml'::text])))
);

CREATE TABLE olp.settings (
    key text NOT NULL,
    value text NOT NULL,
    etag uuid NOT NULL,
    updated_by uuid NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL
);

CREATE TABLE olp.supply_budgets (
    owner_id uuid NOT NULL,
    owner_kind text NOT NULL,
    daily_cost_limit numeric(24,12),
    monthly_cost_limit numeric(24,12),
    CONSTRAINT supply_budgets_check CHECK (((daily_cost_limit IS NOT NULL) OR (monthly_cost_limit IS NOT NULL))),
    CONSTRAINT supply_budgets_daily_cost_limit_check CHECK ((daily_cost_limit > (0)::numeric)),
    CONSTRAINT supply_budgets_monthly_cost_limit_check CHECK ((monthly_cost_limit > (0)::numeric)),
    CONSTRAINT supply_budgets_owner_kind_check CHECK ((owner_kind = ANY (ARRAY['connection'::text, 'slot'::text, 'route'::text])))
);

CREATE TABLE olp.trace_spans (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    project_id uuid NOT NULL,
    api_key_id uuid NOT NULL,
    principal_digest bytea NOT NULL,
    trace_id bytea NOT NULL,
    span_id bytea NOT NULL,
    parent_span_id bytea,
    run_id uuid,
    attempt_id uuid,
    started_nanos numeric(20,0) NOT NULL,
    ended_nanos numeric(20,0) NOT NULL,
    category text NOT NULL,
    status smallint NOT NULL,
    attributes jsonb NOT NULL,
    received_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT trace_spans_attributes_check CHECK ((jsonb_typeof(attributes) = 'object'::text)),
    CONSTRAINT trace_spans_category_check CHECK ((category = ANY (ARRAY['chat'::text, 'embeddings'::text, 'text_completion'::text, 'execute_tool'::text, 'invoke_agent'::text, 'create_agent'::text, 'other'::text]))),
    CONSTRAINT trace_spans_parent_span_id_check CHECK (((parent_span_id IS NULL) OR (octet_length(parent_span_id) = 8))),
    CONSTRAINT trace_spans_principal_digest_check CHECK ((octet_length(principal_digest) = 32)),
    CONSTRAINT trace_spans_span_id_check CHECK ((octet_length(span_id) = 8)),
    CONSTRAINT trace_spans_status_check CHECK (((status >= 0) AND (status <= 2))),
    CONSTRAINT trace_spans_trace_id_check CHECK ((octet_length(trace_id) = 16))
);

CREATE TABLE olp.usage_request_anchors (
    request_id uuid NOT NULL,
    request_started_at timestamp with time zone NOT NULL
);

CREATE TABLE olp.worker_task_health (
    task text NOT NULL,
    checked_at timestamp with time zone NOT NULL,
    last_success_at timestamp with time zone,
    last_progress_at timestamp with time zone,
    successes_total bigint DEFAULT 0 NOT NULL,
    failures_total bigint DEFAULT 0 NOT NULL,
    skipped_total bigint DEFAULT 0 NOT NULL,
    first_seen_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT worker_task_health_failures_total_check CHECK ((failures_total >= 0)),
    CONSTRAINT worker_task_health_skipped_total_check CHECK ((skipped_total >= 0)),
    CONSTRAINT worker_task_health_successes_total_check CHECK ((successes_total >= 0)),
    CONSTRAINT worker_task_health_task_check CHECK ((task = ANY (ARRAY['request_metadata_consumer'::text, 'maintenance'::text, 'cost_reconciliation'::text, 'request_metadata_gateway_epoch_detection'::text, 'media_reconciliation'::text, 'notification_delivery'::text, 'grant_refresh'::text, 'health_probes'::text, 'export_delivery'::text, 'managed_export_delivery'::text, 'focus_export'::text, 'billing_import'::text, 'batch_settlement'::text, 'execution_debits'::text, 'retained_settlement'::text, 'code_allowance_read'::text])))
);

CREATE TABLE olp.workflow_model_leases (
    dispatch_id uuid NOT NULL,
    claim_id uuid NOT NULL,
    expires_at timestamp with time zone NOT NULL
);

CREATE TABLE olp.workflow_model_nonacceptance_proofs (
    dispatch_id uuid NOT NULL,
    claim_id uuid NOT NULL,
    source text NOT NULL,
    observed_at timestamp with time zone DEFAULT clock_timestamp() NOT NULL,
    CONSTRAINT workflow_model_nonacceptance_proofs_source_check CHECK ((source = 'transport_not_sent'::text))
);

CREATE TABLE olp.workload_issuers (
    id uuid NOT NULL,
    issuer text NOT NULL,
    document jsonb NOT NULL,
    created_by uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    etag uuid NOT NULL,
    CONSTRAINT workload_issuers_document_check CHECK ((jsonb_typeof(document) = 'object'::text))
);

ALTER TABLE ONLY olp.requests ATTACH PARTITION olp.requests_default DEFAULT;

ALTER TABLE ONLY olp.a2a_task_admissions
    ADD CONSTRAINT a2a_task_admissions_pkey PRIMARY KEY (dispatch_id);

ALTER TABLE ONLY olp.a2a_task_callback_accounting
    ADD CONSTRAINT a2a_task_callback_accounting_kind_accounting_id_key UNIQUE (kind, accounting_id);

ALTER TABLE ONLY olp.a2a_task_callback_accounting
    ADD CONSTRAINT a2a_task_callback_accounting_pkey PRIMARY KEY (dispatch_id, kind, accounting_id);

ALTER TABLE ONLY olp.a2a_task_capabilities
    ADD CONSTRAINT a2a_task_capabilities_pkey PRIMARY KEY (dispatch_id);

ALTER TABLE ONLY olp.a2a_task_capabilities
    ADD CONSTRAINT a2a_task_capabilities_token_digest_key UNIQUE (token_digest);

ALTER TABLE ONLY olp.a2a_tasks
    ADD CONSTRAINT a2a_tasks_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.agent_approvals
    ADD CONSTRAINT agent_approvals_dispatch_id_key UNIQUE (dispatch_id);

ALTER TABLE ONLY olp.agent_approvals
    ADD CONSTRAINT agent_approvals_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.agent_credentials
    ADD CONSTRAINT agent_credentials_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.agent_delegations
    ADD CONSTRAINT agent_delegations_consent_digest_key UNIQUE (consent_digest);

ALTER TABLE ONLY olp.agent_delegations
    ADD CONSTRAINT agent_delegations_pkey PRIMARY KEY (token_digest);

ALTER TABLE ONLY olp.agent_publication_revisions
    ADD CONSTRAINT agent_publication_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.agent_publication_revisions
    ADD CONSTRAINT agent_publication_revisions_publication_id_digest_key UNIQUE (publication_id, digest);

ALTER TABLE ONLY olp.agent_publication_revisions
    ADD CONSTRAINT agent_publication_revisions_publication_id_id_key UNIQUE (publication_id, id);

ALTER TABLE ONLY olp.agent_publications
    ADD CONSTRAINT agent_publications_id_project_id_kind_key UNIQUE (id, project_id, kind);

ALTER TABLE ONLY olp.agent_publications
    ADD CONSTRAINT agent_publications_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.agent_runs
    ADD CONSTRAINT agent_runs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.agent_steps
    ADD CONSTRAINT agent_steps_dispatch_id_key UNIQUE (dispatch_id);

ALTER TABLE ONLY olp.agent_steps
    ADD CONSTRAINT agent_steps_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.agent_steps
    ADD CONSTRAINT agent_steps_run_id_id_key UNIQUE (run_id, id);

ALTER TABLE ONLY olp.agent_steps
    ADD CONSTRAINT agent_steps_run_id_ordinal_key UNIQUE (run_id, ordinal);

ALTER TABLE ONLY olp.aggregate_budget_accounts
    ADD CONSTRAINT aggregate_budget_accounts_installation_id_key UNIQUE (installation_id);

ALTER TABLE ONLY olp.aggregate_budget_accounts
    ADD CONSTRAINT aggregate_budget_accounts_organization_id_key UNIQUE (organization_id);

ALTER TABLE ONLY olp.aggregate_budget_accounts
    ADD CONSTRAINT aggregate_budget_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.aggregate_cost_windows
    ADD CONSTRAINT aggregate_cost_windows_pkey PRIMARY KEY (account_id, window_kind, window_id);

ALTER TABLE ONLY olp.aggregate_budget_accounts
    ADD CONSTRAINT aggregate_key_route UNIQUE (api_key_id, route_slug);

ALTER TABLE ONLY olp.aggregate_budget_accounts
    ADD CONSTRAINT aggregate_project_attribution UNIQUE (project_id, attribution_key, attribution_value);

ALTER TABLE ONLY olp.api_key_cost_windows
    ADD CONSTRAINT api_key_cost_windows_pkey PRIMARY KEY (api_key_id, window_kind, window_id);

ALTER TABLE ONLY olp.api_key_overlaps
    ADD CONSTRAINT api_key_overlaps_pkey PRIMARY KEY (lookup_id);

ALTER TABLE ONLY olp.api_keys
    ADD CONSTRAINT api_keys_lookup_id_key UNIQUE (lookup_id);

ALTER TABLE ONLY olp.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.async_worker_counters
    ADD CONSTRAINT async_worker_counters_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_pkey PRIMARY KEY (attempt_id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_request_id_attempt_ordinal_key UNIQUE (request_id, attempt_ordinal);

ALTER TABLE ONLY olp.attempt_usage_hourly
    ADD CONSTRAINT attempt_usage_hourly_dimensions_key UNIQUE NULLS NOT DISTINCT (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id, budget_group_id, attribution, model_family, estimate_provenance, end_user_digest, budget_bucket, budget_exempt, commitment_amount, commitment_period, agent_principal_id, delegation_pair_digest, caller_credential);

ALTER TABLE ONLY olp.attempts
    ADD CONSTRAINT attempts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.attempts
    ADD CONSTRAINT attempts_request_id_ordinal_key UNIQUE (request_id, ordinal);

ALTER TABLE ONLY olp.audit
    ADD CONSTRAINT audit_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.auth_admission
    ADD CONSTRAINT auth_admission_pkey PRIMARY KEY (action, digest);

ALTER TABLE ONLY olp.batch_pending_recorded_owners
    ADD CONSTRAINT batch_pending_recorded_owners_pkey PRIMARY KEY (resource_id, ordinal, budget_owner_id);

ALTER TABLE ONLY olp.batch_pending_subjects
    ADD CONSTRAINT batch_pending_subjects_pkey PRIMARY KEY (resource_id, ordinal);

ALTER TABLE ONLY olp.batch_settlement_results
    ADD CONSTRAINT batch_settlement_results_event_id_key UNIQUE (event_id);

ALTER TABLE ONLY olp.batch_settlement_results
    ADD CONSTRAINT batch_settlement_results_pkey PRIMARY KEY (resource_id, custom_id_digest, result_kind);

ALTER TABLE ONLY olp.batch_settlements
    ADD CONSTRAINT batch_settlements_pkey PRIMARY KEY (resource_id);

ALTER TABLE ONLY olp.notification_deliveries
    ADD CONSTRAINT budget_alert_deliveries_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.notification_deliveries
    ADD CONSTRAINT budget_alert_deliveries_rule_id_window_id_threshold_percent_key UNIQUE (rule_id, window_id, threshold_percent);

ALTER TABLE ONLY olp.notification_rules
    ADD CONSTRAINT budget_alert_rules_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.notification_rules
    ADD CONSTRAINT budget_alert_rules_subject_kind_subject_id_window_kind_thre_key UNIQUE (subject_kind, subject_id, window_kind, threshold_percent, destination_id);

ALTER TABLE ONLY olp.budget_calendar_changes
    ADD CONSTRAINT budget_calendar_changes_pkey PRIMARY KEY (window_kind, effective_at);

ALTER TABLE ONLY olp.budget_group_cost_windows
    ADD CONSTRAINT budget_group_cost_windows_pkey PRIMARY KEY (budget_group_id, window_kind, window_id);

ALTER TABLE ONLY olp.budget_groups
    ADD CONSTRAINT budget_groups_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.budget_increase_requests
    ADD CONSTRAINT budget_increase_requests_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.budget_increases
    ADD CONSTRAINT budget_increases_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.capture_configuration
    ADD CONSTRAINT capture_configuration_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp.capture_policies
    ADD CONSTRAINT capture_policies_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.capture_policies
    ADD CONSTRAINT capture_policies_project_id_route_slug_key UNIQUE NULLS NOT DISTINCT (project_id, route_slug);

ALTER TABLE ONLY olp.capture_redactors
    ADD CONSTRAINT capture_redactors_pkey PRIMARY KEY (policy_id, "position");

ALTER TABLE ONLY olp.code_accounts
    ADD CONSTRAINT code_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_accounts
    ADD CONSTRAINT code_accounts_project_id_provider_id_principal_key UNIQUE (project_id, provider_id, principal);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_recovery_from_key UNIQUE (recovery_from);

ALTER TABLE ONLY olp.code_bindings
    ADD CONSTRAINT code_bindings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_bindings
    ADD CONSTRAINT code_bindings_route_id_api_key_id_conversation_key UNIQUE (route_id, api_key_id, conversation);

ALTER TABLE ONLY olp.code_decisions
    ADD CONSTRAINT code_decisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_route_id_api_key_id_conversation_model_key UNIQUE (route_id, api_key_id, conversation, model);

ALTER TABLE ONLY olp.code_paid_references
    ADD CONSTRAINT code_paid_references_pkey PRIMARY KEY (route_id, api_key_id, external_id);

ALTER TABLE ONLY olp.code_pins
    ADD CONSTRAINT code_pins_pkey PRIMARY KEY (root_id, model);

ALTER TABLE ONLY olp.code_pool_accounts
    ADD CONSTRAINT code_pool_accounts_pkey PRIMARY KEY (pool_id, account_id);

ALTER TABLE ONLY olp.code_pool_keys
    ADD CONSTRAINT code_pool_keys_pkey PRIMARY KEY (pool_id, api_key_id);

ALTER TABLE ONLY olp.code_pools
    ADD CONSTRAINT code_pools_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_references
    ADD CONSTRAINT code_references_pkey PRIMARY KEY (route_id, api_key_id, external_id);

ALTER TABLE ONLY olp.code_refusals
    ADD CONSTRAINT code_refusals_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_research_sessions
    ADD CONSTRAINT code_research_sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_route_revisions
    ADD CONSTRAINT code_route_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_route_revisions
    ADD CONSTRAINT code_route_revisions_route_id_revision_key UNIQUE (route_id, revision);

ALTER TABLE ONLY olp.code_routes
    ADD CONSTRAINT code_routes_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_routes
    ADD CONSTRAINT code_routes_slug_key UNIQUE (slug);

ALTER TABLE ONLY olp.code_token_budgets
    ADD CONSTRAINT code_token_budgets_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.code_token_reservations
    ADD CONSTRAINT code_token_reservations_pkey PRIMARY KEY (attempt_id, budget_id, period);

ALTER TABLE ONLY olp.code_token_windows
    ADD CONSTRAINT code_token_windows_pkey PRIMARY KEY (budget_id, period, starts_at);

ALTER TABLE ONLY olp.commitment_cutover
    ADD CONSTRAINT commitment_cutover_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp.commitment_periods
    ADD CONSTRAINT commitment_periods_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.commitment_periods
    ADD CONSTRAINT commitment_periods_provider_id_period_bucket_start_key UNIQUE (provider_id, period, bucket_start);

ALTER TABLE ONLY olp.commitment_periods
    ADD CONSTRAINT commitment_periods_schedule_id_bucket_start_key UNIQUE (schedule_id, bucket_start);

ALTER TABLE ONLY olp.commitment_schedules
    ADD CONSTRAINT commitment_schedules_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.commitment_schedules
    ADD CONSTRAINT commitment_schedules_provider_id_effective_at_key UNIQUE (provider_id, effective_at);

ALTER TABLE ONLY olp.commitment_usage
    ADD CONSTRAINT commitment_usage_pkey PRIMARY KEY (attempt_id);

ALTER TABLE ONLY olp.end_user_accounts
    ADD CONSTRAINT end_user_accounts_api_key_id_project_id_end_user_digest_key UNIQUE NULLS NOT DISTINCT (api_key_id, project_id, end_user_digest);

ALTER TABLE ONLY olp.end_user_accounts
    ADD CONSTRAINT end_user_accounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.end_user_cost_windows
    ADD CONSTRAINT end_user_cost_windows_pkey PRIMARY KEY (account_id, window_kind, window_id);

ALTER TABLE ONLY olp.execution_debit_owners
    ADD CONSTRAINT execution_debit_owners_pkey PRIMARY KEY (dispatch_id, owner_id, basis);

ALTER TABLE ONLY olp.execution_debit_resolutions
    ADD CONSTRAINT execution_debit_resolutions_pkey PRIMARY KEY (dispatch_id);

ALTER TABLE ONLY olp.execution_debit_result_resolutions
    ADD CONSTRAINT execution_debit_result_resolutions_pkey PRIMARY KEY (dispatch_id, resolved_digest);

ALTER TABLE ONLY olp.execution_debit_results
    ADD CONSTRAINT execution_debit_results_dispatch_event_key UNIQUE (dispatch_id, event_id);

ALTER TABLE ONLY olp.execution_debit_results
    ADD CONSTRAINT execution_debit_results_pkey PRIMARY KEY (dispatch_id, result_digest);

ALTER TABLE ONLY olp.execution_debit_versions
    ADD CONSTRAINT execution_debit_versions_pkey PRIMARY KEY (owner_id);

ALTER TABLE ONLY olp.execution_debits
    ADD CONSTRAINT execution_debits_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.export_cursors
    ADD CONSTRAINT export_cursors_pkey PRIMARY KEY (sink_id, stream);

ALTER TABLE ONLY olp.export_gaps
    ADD CONSTRAINT export_gaps_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.export_pending
    ADD CONSTRAINT export_pending_pkey PRIMARY KEY (sink_id, record_id);

ALTER TABLE ONLY olp.export_records
    ADD CONSTRAINT export_records_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.export_records
    ADD CONSTRAINT export_records_stream_source_id_key UNIQUE (stream, source_id);

ALTER TABLE ONLY olp.export_sinks
    ADD CONSTRAINT export_sinks_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.focus_datasets
    ADD CONSTRAINT focus_datasets_pkey PRIMARY KEY (sink_id, charge_date);

ALTER TABLE ONLY olp.focus_dirty_days
    ADD CONSTRAINT focus_dirty_days_pkey PRIMARY KEY (day);

ALTER TABLE ONLY olp.grant_enrollments
    ADD CONSTRAINT grant_enrollments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.guardrail_attachments
    ADD CONSTRAINT guardrail_attachments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.guardrail_credentials
    ADD CONSTRAINT guardrail_credentials_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.guardrail_credentials
    ADD CONSTRAINT guardrail_credentials_project_id_name_key UNIQUE (project_id, name);

ALTER TABLE ONLY olp.guardrail_draft_attachments
    ADD CONSTRAINT guardrail_draft_attachments_pkey PRIMARY KEY (draft_id, "position");

ALTER TABLE ONLY olp.guardrail_policies
    ADD CONSTRAINT guardrail_policies_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.guardrail_policy_revision_activations
    ADD CONSTRAINT guardrail_policy_revision_activations_pkey PRIMARY KEY (revision_id);

ALTER TABLE ONLY olp.guardrail_policy_revisions
    ADD CONSTRAINT guardrail_policy_revisions_id_policy_id_key UNIQUE (id, policy_id);

ALTER TABLE ONLY olp.guardrail_policy_revisions
    ADD CONSTRAINT guardrail_policy_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.guardrail_policy_revisions
    ADD CONSTRAINT guardrail_policy_revisions_policy_id_revision_key UNIQUE (policy_id, revision);

ALTER TABLE ONLY olp.guardrail_revision_activations
    ADD CONSTRAINT guardrail_revision_activations_pkey PRIMARY KEY (revision_id);

ALTER TABLE ONLY olp.guardrail_revisions
    ADD CONSTRAINT guardrail_revisions_guardrail_id_revision_key UNIQUE (guardrail_id, revision);

ALTER TABLE ONLY olp.guardrail_revisions
    ADD CONSTRAINT guardrail_revisions_id_guardrail_id_key UNIQUE (id, guardrail_id);

ALTER TABLE ONLY olp.guardrail_revisions
    ADD CONSTRAINT guardrail_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.guardrails
    ADD CONSTRAINT guardrails_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.installation
    ADD CONSTRAINT installation_id_key UNIQUE (id);

ALTER TABLE ONLY olp.installation
    ADD CONSTRAINT installation_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp.investigations
    ADD CONSTRAINT investigations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.invitations
    ADD CONSTRAINT invitations_digest_key UNIQUE (digest);

ALTER TABLE ONLY olp.invitations
    ADD CONSTRAINT invitations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.managed_export_deliveries
    ADD CONSTRAINT managed_export_deliveries_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.managed_export_deliveries
    ADD CONSTRAINT managed_export_deliveries_sink_id_event_id_key UNIQUE (sink_id, event_id);

ALTER TABLE ONLY olp.managed_export_sinks
    ADD CONSTRAINT managed_export_sinks_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.management_tokens
    ADD CONSTRAINT management_tokens_lookup_id_key UNIQUE (lookup_id);

ALTER TABLE ONLY olp.management_tokens
    ADD CONSTRAINT management_tokens_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.mcp_call_identities
    ADD CONSTRAINT mcp_call_identities_dispatch_id_key UNIQUE (dispatch_id);

ALTER TABLE ONLY olp.mcp_call_identities
    ADD CONSTRAINT mcp_call_identities_pkey PRIMARY KEY (api_key_id, call_digest);

ALTER TABLE ONLY olp.mcp_executable_permissions
    ADD CONSTRAINT mcp_executable_permissions_pkey PRIMARY KEY (executable, digest);

ALTER TABLE ONLY olp.mcp_grant_enrollments
    ADD CONSTRAINT mcp_grant_enrollments_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.mcp_server_revisions
    ADD CONSTRAINT mcp_server_revisions_id_server_id_key UNIQUE (id, server_id);

ALTER TABLE ONLY olp.mcp_server_revisions
    ADD CONSTRAINT mcp_server_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.mcp_server_revisions
    ADD CONSTRAINT mcp_server_revisions_server_id_revision_key UNIQUE (server_id, revision);

ALTER TABLE ONLY olp.mcp_servers
    ADD CONSTRAINT mcp_servers_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_dispatch_id_key UNIQUE (dispatch_id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_binding_digest_key UNIQUE (binding_digest);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_enrollment_id_key UNIQUE (enrollment_id);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.media_jobs
    ADD CONSTRAINT media_jobs_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.metering_delivery_receipts
    ADD CONSTRAINT metering_delivery_receipts_pkey PRIMARY KEY (sink_id, attempt_id);

ALTER TABLE ONLY olp.mfa_challenges
    ADD CONSTRAINT mfa_challenges_digest_key UNIQUE (digest);

ALTER TABLE ONLY olp.mfa_challenges
    ADD CONSTRAINT mfa_challenges_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.mfa_factors
    ADD CONSTRAINT mfa_factors_credential_id_key UNIQUE (credential_id);

ALTER TABLE ONLY olp.mfa_factors
    ADD CONSTRAINT mfa_factors_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.mfa_recovery_codes
    ADD CONSTRAINT mfa_recovery_codes_pkey PRIMARY KEY (user_id, digest);

ALTER TABLE ONLY olp.model_catalog_project_settings
    ADD CONSTRAINT model_catalog_project_settings_pkey PRIMARY KEY (project_id);

ALTER TABLE ONLY olp.model_catalog_route_settings
    ADD CONSTRAINT model_catalog_route_settings_pkey PRIMARY KEY (route_id);

ALTER TABLE ONLY olp.notification_deliveries
    ADD CONSTRAINT notification_deliveries_lapse_key UNIQUE (rule_id, credential_id);

ALTER TABLE ONLY olp.notification_destination_tests
    ADD CONSTRAINT notification_destination_tests_destination_id_action_id_key UNIQUE (destination_id, action_id);

ALTER TABLE ONLY olp.notification_destination_tests
    ADD CONSTRAINT notification_destination_tests_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.notification_destinations
    ADD CONSTRAINT notification_destinations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.notification_signal_states
    ADD CONSTRAINT notification_signal_states_pkey PRIMARY KEY (rule_id, subject);

ALTER TABLE ONLY olp.oidc_configuration
    ADD CONSTRAINT oidc_configuration_id_key UNIQUE (id);

ALTER TABLE ONLY olp.oidc_configuration
    ADD CONSTRAINT oidc_configuration_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp.oidc_flows
    ADD CONSTRAINT oidc_flows_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.oidc_flows
    ADD CONSTRAINT oidc_flows_state_digest_key UNIQUE (state_digest);

ALTER TABLE ONLY olp.oidc_identities
    ADD CONSTRAINT oidc_identities_issuer_subject_key UNIQUE (issuer, subject);

ALTER TABLE ONLY olp.oidc_identities
    ADD CONSTRAINT oidc_identities_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.organization_members
    ADD CONSTRAINT organization_members_pkey PRIMARY KEY (organization_id, user_id);

ALTER TABLE ONLY olp.organizations
    ADD CONSTRAINT organizations_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.pending_resource_recorded_owners
    ADD CONSTRAINT pending_resource_recorded_owners_pkey PRIMARY KEY (resource_id, ordinal, budget_owner_id);

ALTER TABLE ONLY olp.plugins
    ADD CONSTRAINT plugins_pkey PRIMARY KEY (digest);

ALTER TABLE ONLY olp.prices
    ADD CONSTRAINT prices_revision_scope_key UNIQUE NULLS NOT DISTINCT (pricing_revision_id, provider_kind, provider_id, vendor_id, model, operation);

ALTER TABLE ONLY olp.pricing_currency
    ADD CONSTRAINT pricing_currency_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp.pricing_discounts
    ADD CONSTRAINT pricing_discounts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.pricing_revisions
    ADD CONSTRAINT pricing_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.pricing_revisions
    ADD CONSTRAINT pricing_revisions_revision_key UNIQUE (revision);

ALTER TABLE ONLY olp.pricing_source_snapshots
    ADD CONSTRAINT pricing_source_snapshots_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.pricing_source_snapshots
    ADD CONSTRAINT pricing_source_snapshots_source_id_sha256_key UNIQUE (source_id, sha256);

ALTER TABLE ONLY olp.pricing_sources
    ADD CONSTRAINT pricing_sources_name_key UNIQUE (name);

ALTER TABLE ONLY olp.pricing_sources
    ADD CONSTRAINT pricing_sources_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.project_members
    ADD CONSTRAINT project_members_pkey PRIMARY KEY (project_id, user_id);

ALTER TABLE ONLY olp.projects
    ADD CONSTRAINT projects_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.prompt_labels
    ADD CONSTRAINT prompt_labels_pkey PRIMARY KEY (publication_id, label);

ALTER TABLE ONLY olp.provider_billing_contexts
    ADD CONSTRAINT provider_billing_contexts_pkey PRIMARY KEY (provider_id);

ALTER TABLE ONLY olp.provider_billing_days
    ADD CONSTRAINT provider_billing_days_pkey PRIMARY KEY (provider_id, day);

ALTER TABLE ONLY olp.provider_billing_imports
    ADD CONSTRAINT provider_billing_imports_pkey PRIMARY KEY (provider_id);

ALTER TABLE ONLY olp.provider_billing_revisions
    ADD CONSTRAINT provider_billing_revisions_pkey PRIMARY KEY (provider_id, source, scope_digest, period_start);

ALTER TABLE ONLY olp.provider_credentials
    ADD CONSTRAINT provider_credentials_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.provider_credentials
    ADD CONSTRAINT provider_credentials_provider_id_version_key UNIQUE (provider_id, version);

ALTER TABLE ONLY olp.provider_grants
    ADD CONSTRAINT provider_grants_pkey PRIMARY KEY (credential_id);

ALTER TABLE ONLY olp.provider_grants
    ADD CONSTRAINT provider_grants_refresh_token_id_key UNIQUE (refresh_token_id);

ALTER TABLE ONLY olp.provider_invoiced_costs
    ADD CONSTRAINT provider_invoiced_costs_pkey PRIMARY KEY (provider_id, day, line_item, currency);

ALTER TABLE ONLY olp.provider_models
    ADD CONSTRAINT provider_models_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.provider_models
    ADD CONSTRAINT provider_models_provider_id_upstream_model_key UNIQUE (provider_id, upstream_model);

ALTER TABLE ONLY olp.provider_network_credentials
    ADD CONSTRAINT provider_network_credentials_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.provider_network_credentials
    ADD CONSTRAINT provider_network_credentials_provider_id_version_key UNIQUE (provider_id, version);

ALTER TABLE ONLY olp.provider_resource_pending_usage
    ADD CONSTRAINT provider_resource_pending_usage_pkey PRIMARY KEY (resource_id, ordinal);

ALTER TABLE ONLY olp.provider_resources
    ADD CONSTRAINT provider_resources_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.provider_revisions
    ADD CONSTRAINT provider_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.provider_revisions
    ADD CONSTRAINT provider_revisions_provider_id_revision_key UNIQUE (provider_id, revision);

ALTER TABLE ONLY olp.provider_slots
    ADD CONSTRAINT provider_slots_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.provider_slots
    ADD CONSTRAINT provider_slots_provider_id_position_key UNIQUE (provider_id, "position");

ALTER TABLE ONLY olp.providers
    ADD CONSTRAINT providers_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.provisioned_users
    ADD CONSTRAINT provisioned_users_pkey PRIMARY KEY (source, external_id);

ALTER TABLE ONLY olp.provisioned_users
    ADD CONSTRAINT provisioned_users_user_id_key UNIQUE (user_id);

ALTER TABLE ONLY olp.rate_card_prices
    ADD CONSTRAINT rate_card_prices_revision_scope_key UNIQUE NULLS NOT DISTINCT (rate_card_revision_id, provider_kind, vendor_id, model, operation);

ALTER TABLE ONLY olp.rate_card_revisions
    ADD CONSTRAINT rate_card_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.rate_card_revisions
    ADD CONSTRAINT rate_card_revisions_rate_card_id_revision_key UNIQUE (rate_card_id, revision);

ALTER TABLE ONLY olp.rate_cards
    ADD CONSTRAINT rate_cards_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.recent_auth
    ADD CONSTRAINT recent_auth_pkey PRIMARY KEY (digest);

ALTER TABLE ONLY olp.regional_worker_task_health
    ADD CONSTRAINT regional_worker_task_health_pkey PRIMARY KEY (region, task);

ALTER TABLE ONLY olp.replays
    ADD CONSTRAINT replays_pkey PRIMARY KEY (actor, key);

ALTER TABLE ONLY olp.request_metadata_consumer_health
    ADD CONSTRAINT request_metadata_consumer_health_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp.request_metadata_event_receipts
    ADD CONSTRAINT request_metadata_event_receipts_pkey PRIMARY KEY (event_id, request_id);

ALTER TABLE ONLY olp.request_metadata_gap_hourly
    ADD CONSTRAINT request_metadata_gap_hourly_pkey PRIMARY KEY (bucket, gateway_instance, reason);

ALTER TABLE ONLY olp.request_metadata_gateway_epochs
    ADD CONSTRAINT request_metadata_gateway_epochs_pkey PRIMARY KEY (gateway_instance, process_epoch);

ALTER TABLE ONLY olp.request_metadata_ingestion_gaps
    ADD CONSTRAINT request_metadata_ingestion_gaps_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.requests
    ADD CONSTRAINT requests_pkey PRIMARY KEY (id, started_at);

ALTER TABLE ONLY olp.requests_default
    ADD CONSTRAINT requests_default_pkey PRIMARY KEY (id, started_at);

ALTER TABLE ONLY olp.retained_mutations
    ADD CONSTRAINT retained_mutations_pkey PRIMARY KEY (api_key_id, operation, submission_key);

ALTER TABLE ONLY olp.retained_settlements
    ADD CONSTRAINT retained_settlements_pkey PRIMARY KEY (resource_id);

ALTER TABLE ONLY olp.retained_submissions
    ADD CONSTRAINT retained_submissions_api_key_id_operation_submission_key_key UNIQUE (api_key_id, operation, submission_key);

ALTER TABLE ONLY olp.retained_submissions
    ADD CONSTRAINT retained_submissions_pkey PRIMARY KEY (resource_id);

ALTER TABLE ONLY olp.retained_usage_coverage
    ADD CONSTRAINT retained_usage_coverage_pkey PRIMARY KEY (resource_id, component, model, memory_tier, starts_at, ends_at);

ALTER TABLE ONLY olp.retained_usage_evidence
    ADD CONSTRAINT retained_usage_evidence_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.retained_usage_evidence
    ADD CONSTRAINT retained_usage_evidence_resource_id_idempotency_key_key UNIQUE (resource_id, idempotency_key);

ALTER TABLE ONLY olp.retrieval_binding_revisions
    ADD CONSTRAINT retrieval_binding_revisions_binding_id_revision_key UNIQUE (binding_id, revision);

ALTER TABLE ONLY olp.retrieval_binding_revisions
    ADD CONSTRAINT retrieval_binding_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.retrieval_bindings
    ADD CONSTRAINT retrieval_bindings_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.route_drafts
    ADD CONSTRAINT route_drafts_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.route_revisions
    ADD CONSTRAINT route_revisions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.route_revisions
    ADD CONSTRAINT route_revisions_route_id_revision_key UNIQUE (route_id, revision);

ALTER TABLE ONLY olp.route_template_routes
    ADD CONSTRAINT route_template_routes_pkey PRIMARY KEY (template_id, provider_model_id);

ALTER TABLE ONLY olp.route_templates
    ADD CONSTRAINT route_templates_name_key UNIQUE (name);

ALTER TABLE ONLY olp.route_templates
    ADD CONSTRAINT route_templates_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.routes
    ADD CONSTRAINT routes_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.routes
    ADD CONSTRAINT routes_slug_key UNIQUE (slug);

ALTER TABLE ONLY olp.routing_policies
    ADD CONSTRAINT routing_policies_pkey PRIMARY KEY (scope, scope_id);

ALTER TABLE ONLY olp.runtime_install_status
    ADD CONSTRAINT runtime_install_status_pkey PRIMARY KEY (gateway_instance);

ALTER TABLE ONLY olp.runtime_releases
    ADD CONSTRAINT runtime_releases_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.runtime_releases
    ADD CONSTRAINT runtime_releases_sequence_key UNIQUE (sequence);

ALTER TABLE ONLY olp.runtime_shared_quota_retirements
    ADD CONSTRAINT runtime_shared_quota_retirements_pkey PRIMARY KEY (sequence, project_id, group_name);

ALTER TABLE ONLY olp.saml_assertion_replays
    ADD CONSTRAINT saml_assertion_replays_pkey PRIMARY KEY (digest);

ALTER TABLE ONLY olp.saml_configuration
    ADD CONSTRAINT saml_configuration_id_key UNIQUE (id);

ALTER TABLE ONLY olp.saml_configuration
    ADD CONSTRAINT saml_configuration_pkey PRIMARY KEY (singleton);

ALTER TABLE ONLY olp.saml_flows
    ADD CONSTRAINT saml_flows_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.saml_flows
    ADD CONSTRAINT saml_flows_request_id_key UNIQUE (request_id);

ALTER TABLE ONLY olp.saml_flows
    ADD CONSTRAINT saml_flows_state_digest_key UNIQUE (state_digest);

ALTER TABLE ONLY olp.saml_identities
    ADD CONSTRAINT saml_identities_issuer_subject_key UNIQUE (issuer, subject);

ALTER TABLE ONLY olp.saml_identities
    ADD CONSTRAINT saml_identities_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.scim_group_members
    ADD CONSTRAINT scim_group_members_pkey PRIMARY KEY (group_id, user_id);

ALTER TABLE ONLY olp.scim_group_projects
    ADD CONSTRAINT scim_group_projects_pkey PRIMARY KEY (group_id, project_id);

ALTER TABLE ONLY olp.scim_groups
    ADD CONSTRAINT scim_groups_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.scim_users
    ADD CONSTRAINT scim_users_pkey PRIMARY KEY (user_id);

ALTER TABLE ONLY olp.secrets
    ADD CONSTRAINT secrets_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.sessions
    ADD CONSTRAINT sessions_digest_key UNIQUE (digest);

ALTER TABLE ONLY olp.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.settings
    ADD CONSTRAINT settings_pkey PRIMARY KEY (key);

ALTER TABLE ONLY olp.supply_budgets
    ADD CONSTRAINT supply_budgets_pkey PRIMARY KEY (owner_id);

ALTER TABLE ONLY olp.supply_cost_windows
    ADD CONSTRAINT supply_cost_windows_pkey PRIMARY KEY (owner_id, window_kind, window_id);

ALTER TABLE ONLY olp.trace_spans
    ADD CONSTRAINT trace_spans_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.trace_spans
    ADD CONSTRAINT trace_spans_project_id_api_key_id_principal_digest_trace_id_key UNIQUE (project_id, api_key_id, principal_digest, trace_id, span_id);

ALTER TABLE ONLY olp.usage_request_anchors
    ADD CONSTRAINT usage_request_anchors_pkey PRIMARY KEY (request_id, request_started_at);

ALTER TABLE ONLY olp.users
    ADD CONSTRAINT users_email_key UNIQUE (email);

ALTER TABLE ONLY olp.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);

ALTER TABLE ONLY olp.worker_task_health
    ADD CONSTRAINT worker_task_health_pkey PRIMARY KEY (task);

ALTER TABLE ONLY olp.workflow_model_leases
    ADD CONSTRAINT workflow_model_leases_pkey PRIMARY KEY (dispatch_id);

ALTER TABLE ONLY olp.workflow_model_nonacceptance_proofs
    ADD CONSTRAINT workflow_model_nonacceptance_proofs_pkey PRIMARY KEY (dispatch_id);

ALTER TABLE ONLY olp.workload_issuers
    ADD CONSTRAINT workload_issuers_issuer_key UNIQUE (issuer);

ALTER TABLE ONLY olp.workload_issuers
    ADD CONSTRAINT workload_issuers_pkey PRIMARY KEY (id);

CREATE INDEX a2a_task_admissions_owner ON olp.a2a_task_admissions USING btree (api_key_id);

CREATE INDEX a2a_task_capabilities_task_id ON olp.a2a_task_capabilities USING btree (task_id) WHERE (task_id IS NOT NULL);

CREATE INDEX a2a_tasks_active_owner ON olp.a2a_tasks USING btree (api_key_id, state);

CREATE UNIQUE INDEX a2a_tasks_delegated_owner ON olp.a2a_tasks USING btree (project_id, publication_id, delegation_pair_digest, upstream_id) WHERE (delegation_pair_digest <> ''::text);

CREATE UNIQUE INDEX a2a_tasks_key_owner ON olp.a2a_tasks USING btree (api_key_id, project_id, publication_id, upstream_id) WHERE (delegation_pair_digest = ''::text);

CREATE INDEX a2a_tasks_project_id_id ON olp.a2a_tasks USING btree (project_id, id DESC);

CREATE INDEX agent_approvals_expiry ON olp.agent_approvals USING btree (expires_at) WHERE (consumed_at IS NULL);

CREATE INDEX agent_approvals_run_id ON olp.agent_approvals USING btree (run_id, id);

CREATE INDEX agent_delegations_project_id_id ON olp.agent_delegations USING btree (project_id, id DESC);

CREATE UNIQUE INDEX agent_delegations_public_id ON olp.agent_delegations USING btree (id);

CREATE UNIQUE INDEX agent_publications_name ON olp.agent_publications USING btree (project_id, kind, lower(name)) WHERE (NOT retired);

CREATE INDEX agent_runs_project_id ON olp.agent_runs USING btree (project_id, id DESC);

CREATE UNIQUE INDEX aggregate_project_budget ON olp.aggregate_budget_accounts USING btree (project_id) WHERE (attribution_key IS NULL);

CREATE INDEX api_key_overlaps_expiry ON olp.api_key_overlaps USING btree (expires_at);

CREATE INDEX api_key_overlaps_owner ON olp.api_key_overlaps USING btree (api_key_id);

CREATE UNIQUE INDEX api_key_workload_subject ON olp.api_keys USING btree (workload_digest) WHERE (workload_digest IS NOT NULL);

CREATE INDEX attempt_usage_facts_api_key_observed_at_idx ON olp.attempt_usage_facts USING btree (api_key_id, observed_at DESC);

CREATE INDEX attempt_usage_facts_end_user ON olp.attempt_usage_facts USING btree (end_user_digest, observed_at) WHERE (end_user_digest <> ''::text);

CREATE INDEX attempt_usage_facts_event_id_idx ON olp.attempt_usage_facts USING btree (event_id);

CREATE INDEX attempt_usage_facts_model_idx ON olp.attempt_usage_facts USING btree (upstream_model, observed_at DESC);

CREATE INDEX attempt_usage_facts_observed_at_idx ON olp.attempt_usage_facts USING btree (observed_at DESC);

CREATE INDEX attempt_usage_facts_provider_idx ON olp.attempt_usage_facts USING btree (provider_id, observed_at DESC);

CREATE INDEX attempt_usage_facts_request_idx ON olp.attempt_usage_facts USING btree (request_id, request_started_at, attempt_ordinal);

CREATE INDEX attempt_usage_facts_selector_idx ON olp.attempt_usage_facts USING btree (route_slug, selector, observed_at) WHERE (selector IS NOT NULL);

CREATE INDEX attempt_usage_hourly_agent_budget ON olp.attempt_usage_hourly USING btree (agent_principal_id, budget_bucket) WHERE (agent_principal_id IS NOT NULL);

CREATE INDEX attempt_usage_hourly_api_key_bucket_idx ON olp.attempt_usage_hourly USING btree (api_key_id, bucket DESC) WHERE (api_key_id IS NOT NULL);

CREATE INDEX attempt_usage_hourly_commitment_backfill ON olp.attempt_usage_hourly USING btree (bucket) WHERE (commitment_backfill_pending AND (commitment_period IS NOT NULL));

CREATE INDEX attempt_usage_hourly_end_user ON olp.attempt_usage_hourly USING btree (end_user_digest, bucket) WHERE (end_user_digest <> ''::text);

CREATE INDEX attempt_usage_hourly_pair_budget ON olp.attempt_usage_hourly USING btree (delegation_pair_digest, budget_bucket) WHERE (delegation_pair_digest <> ''::text);

CREATE INDEX attempts_completed_provider_idx ON olp.attempts USING btree (completed_at, provider_id) WHERE (completed_at IS NOT NULL);

CREATE INDEX attempts_provider_started_idx ON olp.attempts USING btree (provider_id, started_at DESC);

CREATE INDEX attempts_request_id_idx ON olp.attempts USING btree (request_id);

CREATE INDEX attempts_routing_measurements ON olp.attempts USING btree (completed_at DESC) WHERE ((error_class IS NULL) AND ((status_code >= 200) AND (status_code <= 299)));

CREATE INDEX audit_time ON olp.audit USING btree (occurred_at DESC, id DESC);

CREATE INDEX auth_admission_expiry ON olp.auth_admission USING btree (window_started_at);

CREATE INDEX batch_pending_usage_attribution ON olp.batch_pending_subjects USING gin (attribution jsonb_path_ops);

CREATE INDEX batch_pending_usage_group ON olp.batch_pending_subjects USING btree (budget_group_id);

CREATE INDEX batch_pending_usage_key ON olp.batch_pending_subjects USING btree (api_key_id, route_slug);

CREATE INDEX batch_pending_usage_key_digest ON olp.batch_pending_subjects USING btree (api_key_id, end_user_digest);

CREATE INDEX batch_pending_usage_key_project ON olp.batch_pending_subjects USING btree (api_key_project_id, end_user_digest);

CREATE INDEX batch_pending_usage_leg_route ON olp.batch_pending_subjects USING btree (leg_route_id);

CREATE INDEX batch_pending_usage_pinned_route ON olp.batch_pending_subjects USING btree (pinned_route_id);

CREATE INDEX batch_pending_usage_project ON olp.batch_pending_subjects USING btree (project_id);

CREATE INDEX batch_pending_usage_provider ON olp.batch_pending_subjects USING btree (provider_id);

CREATE INDEX batch_pending_usage_recorded_owners ON olp.batch_pending_recorded_owners USING btree (budget_owner_id);

CREATE INDEX batch_pending_usage_route ON olp.batch_pending_subjects USING btree (route_id);

CREATE INDEX batch_pending_usage_slot ON olp.batch_pending_subjects USING btree (slot_id);

CREATE INDEX batch_settlements_closed ON olp.batch_settlements USING btree (closed_at) WHERE (closed_at IS NOT NULL);

CREATE INDEX batch_settlements_due ON olp.batch_settlements USING btree (next_attempt_at, resource_id) WHERE (state = ANY (ARRAY['submitting'::text, 'submission_unknown'::text, 'active'::text, 'settling'::text, 'conflict'::text]));

CREATE INDEX batch_settlements_owner ON olp.batch_settlements USING btree (api_key_id, created_at DESC);

CREATE INDEX batch_settlements_pending_gemini ON olp.batch_settlements USING btree (api_key_id, provider_id, slot_id, credential_id, resource_id) WHERE ((adapter = 'gemini_managed'::text) AND (closed_at IS NULL));

CREATE UNIQUE INDEX batch_settlements_submission_identity ON olp.batch_settlements USING btree (api_key_id, submission_digest) WHERE ((state = ANY (ARRAY['submitting'::text, 'submission_unknown'::text])) OR ((state = ANY (ARRAY['conflict'::text, 'uncertain'::text])) AND (upstream_id IS NULL)));

CREATE INDEX batch_settlements_unmapped_submission_source ON olp.batch_settlements USING btree (provider_id, slot_id, submission_source, credential_id) WHERE (upstream_id IS NULL);

CREATE INDEX batch_settlements_upstream_source ON olp.batch_settlements USING btree (provider_id, slot_id, upstream_id, credential_id) WHERE (upstream_id IS NOT NULL);

CREATE UNIQUE INDEX budget_groups_name_scope ON olp.budget_groups USING btree (lower(name), COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid)) WHERE (retired_at IS NULL);

CREATE UNIQUE INDEX budget_increase_requests_increase ON olp.budget_increase_requests USING btree (increase_id) WHERE (increase_id IS NOT NULL);

CREATE INDEX budget_increase_requests_pending ON olp.budget_increase_requests USING btree (owner_id, window_kind, window_id) WHERE (status = 'pending'::text);

CREATE INDEX budget_increase_requests_project ON olp.budget_increase_requests USING btree (project_id, id DESC);

CREATE INDEX budget_increases_active_owner ON olp.budget_increases USING btree (owner_id, expires_at) WHERE (revoked_at IS NULL);

CREATE INDEX budget_increases_organization ON olp.budget_increases USING btree (organization_id, id DESC) WHERE (organization_id IS NOT NULL);

CREATE INDEX budget_increases_project ON olp.budget_increases USING btree (project_id, id DESC);

CREATE INDEX code_account_cooldowns_account ON olp.code_account_cooldowns USING btree (account_id);

CREATE UNIQUE INDEX code_account_cooldowns_pk ON olp.code_account_cooldowns USING btree (account_id, COALESCE(model, ''::text));

CREATE INDEX code_attempts_active_generation ON olp.code_attempts USING btree (account_id, created_at) WHERE ((account_id IS NOT NULL) AND (finished_at IS NULL) AND (operation <> 'prewarm'::text));

CREATE INDEX code_attempts_end_user ON olp.code_attempts USING btree (project_id, end_user_digest, id DESC) WHERE (end_user_digest <> ''::text);

CREATE INDEX code_bindings_root ON olp.code_bindings USING btree (root_id);

CREATE INDEX code_decisions_account_activity ON olp.code_decisions USING btree (chosen_account_id, created_at DESC);

CREATE UNIQUE INDEX code_decisions_attempt ON olp.code_decisions USING btree (attempt_id) WHERE (attempt_id IS NOT NULL);

CREATE INDEX code_decisions_project ON olp.code_decisions USING btree (project_id, id);

CREATE INDEX code_paid_pending_owners ON olp.code_attempts USING gin (paid_cost_owners) WHERE ((paid_binding_id IS NOT NULL) AND (state <> ALL (ARRAY['prepared'::text, 'aborted'::text])));

CREATE INDEX code_pins_account ON olp.code_pins USING btree (account_id);

CREATE INDEX code_refusals_end_user ON olp.code_refusals USING btree (project_id, end_user_digest, id DESC) WHERE (end_user_digest <> ''::text);

CREATE INDEX code_research_sessions_expiry ON olp.code_research_sessions USING btree (expires_at);

CREATE INDEX code_research_sessions_key ON olp.code_research_sessions USING btree (api_key_id);

CREATE INDEX commitment_periods_date ON olp.commitment_periods USING btree (bucket_start);

CREATE INDEX commitment_schedules_provider ON olp.commitment_schedules USING btree (provider_id, effective_at DESC);

CREATE INDEX commitment_usage_observed ON olp.commitment_usage USING btree (observed_at);

CREATE INDEX commitment_usage_period ON olp.commitment_usage USING btree (period_id, observed_at, attempt_id);

CREATE INDEX execution_debit_owners_lookup ON olp.execution_debit_owners USING btree (owner_id, dispatch_id);

CREATE INDEX execution_debits_open ON olp.execution_debits USING btree (created_at, id) WHERE (closed_at IS NULL);

CREATE INDEX execution_debits_owner ON olp.execution_debits USING btree (api_key_id, created_at, id);

CREATE INDEX execution_debits_prepared ON olp.execution_debits USING btree (prepared_until, id) WHERE (state = ANY (ARRAY['prepared'::text, 'ready'::text]));

CREATE INDEX export_gaps_sink ON olp.export_gaps USING btree (sink_id, recorded_at DESC, id DESC);

CREATE INDEX export_pending_due ON olp.export_pending USING btree (sink_id, next_attempt_at, record_id) WHERE ((delivered_at IS NULL) AND (rejected_at IS NULL));

CREATE INDEX export_records_age ON olp.export_records USING btree (occurred_at, id);

CREATE UNIQUE INDEX export_sinks_name ON olp.export_sinks USING btree (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), name);

CREATE INDEX focus_datasets_due ON olp.focus_datasets USING btree (sink_id, next_attempt_at);

CREATE INDEX grant_enrollments_expiry ON olp.grant_enrollments USING btree (expires_at);

CREATE UNIQUE INDEX guardrail_attachment_order ON olp.guardrail_attachments USING btree (scope, scope_id, "position") NULLS NOT DISTINCT;

CREATE UNIQUE INDEX guardrail_policies_project_name ON olp.guardrail_policies USING btree (project_id, lower(name)) WHERE (retired_at IS NULL);

CREATE UNIQUE INDEX guardrails_project_name ON olp.guardrails USING btree (project_id, lower(name)) WHERE (retired_at IS NULL);

CREATE INDEX investigations_project_id ON olp.investigations USING btree (project_id, id DESC);

CREATE INDEX invitations_email ON olp.invitations USING btree (email) WHERE ((accepted_at IS NULL) AND (revoked_at IS NULL));

CREATE INDEX managed_export_deliveries_due ON olp.managed_export_deliveries USING btree (next_attempt_at, id) WHERE (status = 'pending'::text);

CREATE INDEX managed_export_deliveries_expiry ON olp.managed_export_deliveries USING btree (expires_at, id);

CREATE UNIQUE INDEX managed_export_sinks_active_name ON olp.managed_export_sinks USING btree (COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), lower(name)) WHERE (retired_at IS NULL);

CREATE INDEX mcp_call_identities_expiry ON olp.mcp_call_identities USING btree (expires_at);

CREATE INDEX mcp_grant_enrollments_project ON olp.mcp_grant_enrollments USING btree (project_id, created_at DESC, id DESC);

CREATE UNIQUE INDEX mcp_servers_project_name ON olp.mcp_servers USING btree (project_id, lower(name)) WHERE (retired_at IS NULL);

CREATE INDEX mcp_tool_calls_key_created ON olp.mcp_tool_calls USING btree (api_key_id, created_at DESC, id DESC);

CREATE INDEX mcp_tool_calls_project_created ON olp.mcp_tool_calls USING btree (project_id, created_at DESC, id DESC);

CREATE INDEX mcp_user_grants_project ON olp.mcp_user_grants USING btree (project_id, created_at DESC, id DESC);

CREATE INDEX media_jobs_api_key_created_idx ON olp.media_jobs USING btree (api_key_id, created_at DESC, id DESC);

CREATE INDEX media_jobs_created_idx ON olp.media_jobs USING btree (created_at DESC, id DESC);

CREATE INDEX media_jobs_provider_live_idx ON olp.media_jobs USING btree (provider_id, provider_revision_id) WHERE (lifecycle_state <> 'deleted'::text);

CREATE INDEX media_jobs_reconciliation_due_idx ON olp.media_jobs USING btree (next_reconciliation_at, created_at, id) WHERE (lifecycle_state <> 'deleted'::text);

CREATE INDEX media_jobs_reconciliation_idx ON olp.media_jobs USING btree (lifecycle_state, updated_at, id) WHERE (lifecycle_state <> ALL (ARRAY['active'::text, 'deleted'::text]));

CREATE INDEX media_jobs_state_created_idx ON olp.media_jobs USING btree (state, created_at DESC, id DESC);

CREATE UNIQUE INDEX media_jobs_upstream_unique_idx ON olp.media_jobs USING btree (provider_id, upstream_job_id) WHERE (upstream_job_id IS NOT NULL);

CREATE INDEX metering_delivery_receipts_settled ON olp.metering_delivery_receipts USING btree (settled_at, sink_id, attempt_id);

CREATE INDEX mfa_challenge_expiry ON olp.mfa_challenges USING btree (expires_at);

CREATE INDEX mfa_challenge_user ON olp.mfa_challenges USING btree (user_id, created_at);

CREATE UNIQUE INDEX mfa_one_totp ON olp.mfa_factors USING btree (user_id) WHERE (kind = 'totp'::text);

CREATE INDEX mfa_user_factors ON olp.mfa_factors USING btree (user_id);

CREATE UNIQUE INDEX notification_deliveries_key_due ON olp.notification_deliveries USING btree (rule_id, api_key_id, reason, due_at) WHERE (api_key_id IS NOT NULL);

CREATE UNIQUE INDEX notification_deliveries_signal ON olp.notification_deliveries USING btree (rule_id, dedup_key, resolved) WHERE (dedup_key IS NOT NULL);

CREATE INDEX notification_destination_tests_recent ON olp.notification_destination_tests USING btree (destination_id, claimed_at);

CREATE UNIQUE INDEX notification_destinations_name_scope ON olp.notification_destinations USING btree (lower(name), COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid));

CREATE UNIQUE INDEX notification_rules_key_event ON olp.notification_rules USING btree (event, subject_id, destination_id) WHERE (event = 'key.expiring'::text);

CREATE UNIQUE INDEX notification_rules_provider_event ON olp.notification_rules USING btree (event, destination_id) WHERE (event = 'provider.grant.lapsed'::text);

CREATE INDEX oidc_identities_user ON olp.oidc_identities USING btree (user_id);

CREATE UNIQUE INDEX organizations_name ON olp.organizations USING btree (lower(name));

CREATE INDEX pending_usage_attribution ON olp.provider_resource_pending_usage USING gin (attribution jsonb_path_ops);

CREATE INDEX pending_usage_group ON olp.provider_resource_pending_usage USING btree (budget_group_id);

CREATE INDEX pending_usage_key ON olp.provider_resource_pending_usage USING btree (api_key_id, route_slug);

CREATE INDEX pending_usage_key_digest ON olp.provider_resource_pending_usage USING btree (api_key_id, end_user_digest);

CREATE INDEX pending_usage_key_project ON olp.provider_resource_pending_usage USING btree (api_key_project_id, end_user_digest);

CREATE INDEX pending_usage_leg_route ON olp.provider_resource_pending_usage USING btree (leg_route_id);

CREATE INDEX pending_usage_pinned_route ON olp.provider_resource_pending_usage USING btree (pinned_route_id);

CREATE INDEX pending_usage_project ON olp.provider_resource_pending_usage USING btree (project_id);

CREATE INDEX pending_usage_provider ON olp.provider_resource_pending_usage USING btree (provider_id);

CREATE INDEX pending_usage_recorded_owners ON olp.pending_resource_recorded_owners USING btree (budget_owner_id);

CREATE INDEX pending_usage_route ON olp.provider_resource_pending_usage USING btree (route_id);

CREATE INDEX pending_usage_slot ON olp.provider_resource_pending_usage USING btree (slot_id);

CREATE INDEX prices_provider_override_idx ON olp.prices USING btree (provider_id, model, operation) WHERE (provider_id IS NOT NULL);

CREATE INDEX pricing_discounts_scope_effective ON olp.pricing_discounts USING btree (provider_kind, vendor_id, provider_id, effective_at DESC);

CREATE UNIQUE INDEX projects_name ON olp.projects USING btree (lower(name));

CREATE INDEX projects_organization ON olp.projects USING btree (organization_id) WHERE (organization_id IS NOT NULL);

CREATE INDEX provider_billing_days_day_idx ON olp.provider_billing_days USING btree (day);

CREATE INDEX provider_grants_refresh ON olp.provider_grants USING btree (refresh_at) WHERE (refresh_token_id IS NOT NULL);

CREATE INDEX provider_resources_credential ON olp.provider_resources USING btree (credential_id, provider_id, created_at DESC, id DESC) WHERE ((credential_id IS NOT NULL) AND (state <> 'deleted'::text));

CREATE INDEX provider_resources_expiry ON olp.provider_resources USING btree (expires_at, id) WHERE (expires_at IS NOT NULL);

CREATE INDEX provider_resources_live_file_by_upstream ON olp.provider_resources USING btree (api_key_id, provider_id, slot_id, upstream_id, credential_id) WHERE ((kind = 'file'::text) AND (state <> 'deleted'::text));

CREATE UNIQUE INDEX provider_resources_native_identity ON olp.provider_resources USING btree (kind, api_key_id, provider_id, provider_revision_id, route_slug, route_revision_id, target_id, slot_id, credential_id, upstream_id, (
CASE
    WHEN ((state = 'deleted'::text) AND (kind = ANY (ARRAY['file'::text, 'strict_file'::text]))) THEN id
    ELSE NULL::uuid
END)) NULLS NOT DISTINCT WHERE (kind = ANY (ARRAY['file'::text, 'batch'::text, 'response'::text, 'continuation'::text, 'strict_response'::text, 'interaction'::text, 'strict_file'::text, 'strict_batch'::text, 'anthropic_file'::text, 'anthropic_batch'::text, 'gemini_upload'::text, 'gemini_file'::text, 'gemini_batch'::text, 'cloud_batch'::text]));

CREATE INDEX provider_resources_owner ON olp.provider_resources USING btree (api_key_id, kind, created_at DESC, id DESC);

CREATE INDEX provider_resources_parent ON olp.provider_resources USING btree (api_key_id, parent_id) WHERE (parent_id IS NOT NULL);

CREATE UNIQUE INDEX provider_resources_submission_identity ON olp.provider_resources USING btree (api_key_id, submission_id) WHERE (submission_id IS NOT NULL);

CREATE UNIQUE INDEX provider_slots_default ON olp.provider_slots USING btree (provider_id) WHERE is_default;

CREATE UNIQUE INDEX providers_name ON olp.providers USING btree (lower(name));

CREATE INDEX rate_card_revisions_effective ON olp.rate_card_revisions USING btree (rate_card_id, effective_at DESC);

CREATE UNIQUE INDEX rate_cards_owner_key ON olp.rate_cards USING btree (COALESCE((organization_id)::text, ''::text), COALESCE((project_id)::text, ''::text));

CREATE UNIQUE INDEX request_metadata_event_receipts_event_identity_idx ON olp.request_metadata_event_receipts USING btree (event_id);

CREATE INDEX request_metadata_event_receipts_recorded_at_idx ON olp.request_metadata_event_receipts USING brin (recorded_at);

CREATE UNIQUE INDEX request_metadata_event_receipts_request_identity_idx ON olp.request_metadata_event_receipts USING btree (request_id);

CREATE INDEX request_metadata_gap_hourly_overlap_idx ON olp.request_metadata_gap_hourly USING btree (last_observed_at, first_observed_at);

CREATE UNIQUE INDEX request_metadata_gateway_epochs_one_open_idx ON olp.request_metadata_gateway_epochs USING btree (gateway_instance) WHERE ((gracefully_closed_at IS NULL) AND (stale_detected_at IS NULL));

CREATE UNIQUE INDEX request_metadata_gateway_epochs_process_epoch_idx ON olp.request_metadata_gateway_epochs USING btree (process_epoch);

CREATE INDEX request_metadata_gateway_epochs_stale_scan_idx ON olp.request_metadata_gateway_epochs USING btree (updated_at) WHERE ((gracefully_closed_at IS NULL) AND (stale_detected_at IS NULL));

CREATE INDEX request_metadata_gateway_epochs_unresolved_idx ON olp.request_metadata_gateway_epochs USING btree (stale_detected_at) WHERE ((stale_detected_at IS NOT NULL) AND (acknowledged_at IS NULL));

CREATE UNIQUE INDEX request_metadata_ingestion_gaps_deduplication_key_idx ON olp.request_metadata_ingestion_gaps USING btree (deduplication_key) WHERE (deduplication_key IS NOT NULL);

CREATE INDEX requests_cache_result_idx ON ONLY olp.requests USING btree (started_at, cache_result) WHERE (cache_result IS NOT NULL);

CREATE INDEX requests_completed_route_idx ON ONLY olp.requests USING btree (completed_at, route_slug) WHERE (completed_at IS NOT NULL);

CREATE INDEX requests_default_completed_at_route_slug_idx ON olp.requests_default USING btree (completed_at, route_slug) WHERE (completed_at IS NOT NULL);

CREATE INDEX requests_parent_idx ON ONLY olp.requests USING btree (parent_request_id) WHERE (parent_request_id IS NOT NULL);

CREATE INDEX requests_default_parent_request_id_idx ON olp.requests_default USING btree (parent_request_id) WHERE (parent_request_id IS NOT NULL);

CREATE INDEX requests_route_idx ON ONLY olp.requests USING btree (route_slug, started_at DESC);

CREATE INDEX requests_default_route_slug_started_at_idx ON olp.requests_default USING btree (route_slug, started_at DESC);

CREATE INDEX requests_default_started_at_cache_result_idx ON olp.requests_default USING btree (started_at, cache_result) WHERE (cache_result IS NOT NULL);

CREATE INDEX requests_started_at_idx ON ONLY olp.requests USING btree (started_at DESC);

CREATE INDEX requests_default_started_at_idx ON olp.requests_default USING btree (started_at DESC);

CREATE INDEX retained_keys_current_group ON olp.api_keys USING btree (budget_group_id, id) WHERE (budget_group_id IS NOT NULL);

CREATE INDEX retained_mutations_unresolved_resource ON olp.retained_mutations USING btree (resource_id) WHERE (state = ANY (ARRAY['dispatched'::text, 'submission_unknown'::text]));

CREATE UNIQUE INDEX retained_resources_native_identity ON olp.provider_resources USING btree (kind, api_key_id, provider_id, provider_revision_id, route_slug, route_revision_id, target_id, slot_id, credential_id, upstream_id, parent_id, (
CASE
    WHEN ((state = 'deleted'::text) AND (kind = 'vector_store'::text)) THEN id
    ELSE NULL::uuid
END)) NULLS NOT DISTINCT WHERE (kind <> ALL (ARRAY['file'::text, 'batch'::text, 'response'::text, 'continuation'::text, 'strict_response'::text, 'interaction'::text, 'strict_file'::text, 'strict_batch'::text, 'anthropic_file'::text, 'anthropic_batch'::text, 'gemini_upload'::text, 'gemini_file'::text, 'gemini_batch'::text, 'cloud_batch'::text]));

CREATE INDEX retained_settlements_due ON olp.retained_settlements USING btree (poll_after, resource_id) WHERE (state <> 'closed'::text);

CREATE INDEX retained_usage_coverage_intervals ON olp.retained_usage_coverage USING btree (resource_id, component, model, memory_tier, starts_at, ends_at);

CREATE UNIQUE INDEX retrieval_binding_name ON olp.retrieval_bindings USING btree (project_id, lower(name)) WHERE (retired_at IS NULL);

CREATE INDEX retrieval_binding_project ON olp.retrieval_bindings USING btree (project_id, id);

CREATE INDEX route_template_routes_slug_idx ON olp.route_template_routes USING btree (template_id, route_slug);

CREATE INDEX saml_assertion_expiry ON olp.saml_assertion_replays USING btree (expires_at);

CREATE INDEX saml_flow_expiry ON olp.saml_flows USING btree (expires_at);

CREATE INDEX saml_user_identities ON olp.saml_identities USING btree (user_id);

CREATE INDEX scim_group_members_user ON olp.scim_group_members USING btree (user_id);

CREATE UNIQUE INDEX scim_groups_external ON olp.scim_groups USING btree (((document ->> 'externalId'::text))) WHERE ((document ->> 'externalId'::text) IS NOT NULL);

CREATE UNIQUE INDEX scim_groups_name ON olp.scim_groups USING btree (lower((document ->> 'displayName'::text)));

CREATE UNIQUE INDEX scim_users_external ON olp.scim_users USING btree (((document ->> 'externalId'::text))) WHERE ((document ->> 'externalId'::text) IS NOT NULL);

CREATE INDEX secrets_expiry ON olp.secrets USING btree (expires_at, id) WHERE (expires_at IS NOT NULL);

CREATE INDEX sessions_expiry ON olp.sessions USING btree (expires_at, id);

CREATE INDEX sessions_user ON olp.sessions USING btree (user_id);

CREATE INDEX trace_spans_project_received ON olp.trace_spans USING btree (project_id, received_at);

CREATE INDEX trace_spans_received ON olp.trace_spans USING btree (received_at);

CREATE INDEX trace_spans_run_id ON olp.trace_spans USING btree (run_id, started_nanos DESC, id DESC) WHERE (run_id IS NOT NULL);

CREATE INDEX usage_request_anchors_started_at_idx ON olp.usage_request_anchors USING btree (request_started_at);

CREATE INDEX workflow_model_leases_expiry_idx ON olp.workflow_model_leases USING btree (expires_at);

ALTER INDEX olp.requests_completed_route_idx ATTACH PARTITION olp.requests_default_completed_at_route_slug_idx;

ALTER INDEX olp.requests_parent_idx ATTACH PARTITION olp.requests_default_parent_request_id_idx;

ALTER INDEX olp.requests_pkey ATTACH PARTITION olp.requests_default_pkey;

ALTER INDEX olp.requests_route_idx ATTACH PARTITION olp.requests_default_route_slug_started_at_idx;

ALTER INDEX olp.requests_cache_result_idx ATTACH PARTITION olp.requests_default_started_at_cache_result_idx;

ALTER INDEX olp.requests_started_at_idx ATTACH PARTITION olp.requests_default_started_at_idx;

CREATE CONSTRAINT TRIGGER agent_approval_dispatch AFTER INSERT OR UPDATE ON olp.agent_approvals DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION olp.check_approval_dispatch();

CREATE TRIGGER agent_approvals_once BEFORE UPDATE ON olp.agent_approvals FOR EACH ROW EXECUTE FUNCTION olp.guard_agent_approval();

CREATE TRIGGER agent_publication_revisions_immutable BEFORE DELETE OR UPDATE ON olp.agent_publication_revisions FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER agent_runs_immutable BEFORE UPDATE ON olp.agent_runs FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER agent_steps_immutable BEFORE UPDATE ON olp.agent_steps FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER batch_pending_recorded_owners AFTER INSERT ON olp.batch_pending_subjects FOR EACH ROW EXECUTE FUNCTION olp.project_batch_pending_recorded_owners();

CREATE TRIGGER capture_commitment_usage BEFORE INSERT OR UPDATE OF estimated_cost, commitment_weight, usage_complete ON olp.attempt_usage_facts FOR EACH ROW EXECUTE FUNCTION olp.capture_commitment_usage();

CREATE TRIGGER check_route_revision_identity BEFORE INSERT OR UPDATE ON olp.route_revisions FOR EACH ROW EXECUTE FUNCTION olp.check_route_revision_identity();

CREATE TRIGGER commitment_period_dirty AFTER INSERT ON olp.commitment_periods FOR EACH ROW EXECUTE FUNCTION olp.commitment_period_dirty();

CREATE TRIGGER commitment_period_identity BEFORE INSERT ON olp.commitment_periods FOR EACH ROW EXECUTE FUNCTION olp.commitment_period_identity();

CREATE TRIGGER credential_slot_etag BEFORE UPDATE ON olp.provider_slots FOR EACH ROW EXECUTE FUNCTION olp.advance_credential_slot_etag();

CREATE TRIGGER default_budget_bucket BEFORE INSERT ON olp.attempt_usage_hourly FOR EACH ROW EXECUTE FUNCTION olp.default_budget_bucket();

CREATE TRIGGER dirty_commitment_observed_days AFTER INSERT OR UPDATE OF weight, eligible, complete, payload ON olp.commitment_usage FOR EACH ROW EXECUTE FUNCTION olp.dirty_commitment_observed_days();

CREATE TRIGGER execution_debit_owners_immutable BEFORE DELETE OR UPDATE ON olp.execution_debit_owners FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER execution_debit_resolution_immutable BEFORE DELETE OR UPDATE ON olp.execution_debit_resolutions FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER execution_debit_result_resolutions_immutable BEFORE DELETE OR UPDATE ON olp.execution_debit_result_resolutions FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER execution_debit_results_immutable BEFORE DELETE OR UPDATE ON olp.execution_debit_results FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER execution_debits_immutable BEFORE UPDATE ON olp.execution_debits FOR EACH ROW EXECUTE FUNCTION olp.guard_execution_debit();

CREATE TRIGGER export_accounted_request AFTER UPDATE OF status ON olp.request_metadata_event_receipts FOR EACH ROW EXECUTE FUNCTION olp.export_accounted_request();

CREATE TRIGGER export_attempt_metadata AFTER INSERT OR UPDATE OF estimated_cost, unpriced, usage_complete, input_tokens, output_tokens, cached_input_tokens ON olp.attempt_usage_facts FOR EACH ROW EXECUTE FUNCTION olp.export_attempt_metadata();

CREATE TRIGGER export_audit_metadata AFTER INSERT ON olp.audit FOR EACH ROW EXECUTE FUNCTION olp.export_audit_metadata();

CREATE TRIGGER export_audit_record AFTER INSERT ON olp.audit FOR EACH ROW EXECUTE FUNCTION olp.export_audit_record();

CREATE TRIGGER export_client_trace_span AFTER INSERT ON olp.trace_spans FOR EACH ROW EXECUTE FUNCTION olp.export_client_trace_span();

CREATE TRIGGER export_managed_run_evidence AFTER INSERT ON olp.agent_runs FOR EACH ROW EXECUTE FUNCTION olp.enqueue_managed_workflow_evidence();

CREATE TRIGGER export_managed_trace_evidence AFTER INSERT ON olp.trace_spans FOR EACH ROW EXECUTE FUNCTION olp.enqueue_managed_workflow_evidence();

CREATE TRIGGER export_request_metadata AFTER INSERT ON olp.requests FOR EACH ROW EXECUTE FUNCTION olp.export_request_metadata();

CREATE TRIGGER export_workflow_run AFTER INSERT ON olp.agent_runs FOR EACH ROW EXECUTE FUNCTION olp.export_workflow_run();

CREATE TRIGGER guardrail_activation_immutable BEFORE DELETE OR UPDATE ON olp.guardrail_revision_activations FOR EACH ROW EXECUTE FUNCTION olp.guardrail_revision_immutable();

CREATE TRIGGER guardrail_policy_activation_immutable BEFORE DELETE OR UPDATE ON olp.guardrail_policy_revision_activations FOR EACH ROW EXECUTE FUNCTION olp.guardrail_revision_immutable();

CREATE TRIGGER guardrail_policy_revision_immutable BEFORE DELETE OR UPDATE ON olp.guardrail_policy_revisions FOR EACH ROW EXECUTE FUNCTION olp.guardrail_revision_immutable();

CREATE TRIGGER guardrail_revision_immutable BEFORE DELETE OR UPDATE ON olp.guardrail_revisions FOR EACH ROW EXECUTE FUNCTION olp.guardrail_revision_immutable();

CREATE TRIGGER immutable_credential_reference BEFORE UPDATE OF external_reference ON olp.provider_credentials FOR EACH ROW EXECUTE FUNCTION olp.immutable_credential_reference();

CREATE TRIGGER mcp_revision_immutable BEFORE DELETE OR UPDATE ON olp.mcp_server_revisions FOR EACH ROW EXECUTE FUNCTION olp.mcp_revision_immutable();

CREATE TRIGGER mcp_tool_calls_guarded BEFORE DELETE OR UPDATE ON olp.mcp_tool_calls FOR EACH ROW EXECUTE FUNCTION olp.guard_mcp_tool_call();

CREATE TRIGGER media_jobs_lifecycle_guard BEFORE UPDATE ON olp.media_jobs FOR EACH ROW EXECUTE FUNCTION olp.enforce_media_job_lifecycle_transition();

CREATE TRIGGER media_jobs_transition_guard BEFORE UPDATE ON olp.media_jobs FOR EACH ROW EXECUTE FUNCTION olp.enforce_media_job_transition();

CREATE TRIGGER notification_budget_subject_snapshot BEFORE INSERT ON olp.notification_deliveries FOR EACH ROW EXECUTE FUNCTION olp.snapshot_budget_notification_subject();

CREATE TRIGGER pending_recorded_owners AFTER INSERT ON olp.provider_resource_pending_usage FOR EACH ROW EXECUTE FUNCTION olp.project_pending_recorded_owners();

CREATE TRIGGER preserve_code_account BEFORE UPDATE ON olp.code_accounts FOR EACH ROW EXECUTE FUNCTION olp.preserve_code_account();

CREATE TRIGGER preserve_code_binding BEFORE DELETE OR UPDATE ON olp.code_bindings FOR EACH ROW EXECUTE FUNCTION olp.preserve_code_binding();

CREATE TRIGGER preserve_code_pin BEFORE DELETE OR UPDATE ON olp.code_pins FOR EACH ROW EXECUTE FUNCTION olp.preserve_code_pin();

CREATE TRIGGER preserve_provider_credential BEFORE UPDATE ON olp.provider_credentials FOR EACH ROW EXECUTE FUNCTION olp.preserve_provider_credential();

CREATE TRIGGER preserve_route_slug BEFORE UPDATE ON olp.routes FOR EACH ROW EXECUTE FUNCTION olp.preserve_route_slug();

CREATE TRIGGER project_batch_settlement_subjects AFTER INSERT OR UPDATE OF state, template ON olp.batch_settlements FOR EACH ROW EXECUTE FUNCTION olp.project_batch_settlement_subjects();

CREATE TRIGGER project_pending_resource_usage AFTER INSERT OR UPDATE OF metadata, kind ON olp.provider_resources FOR EACH ROW EXECUTE FUNCTION olp.project_pending_resource_usage();

CREATE TRIGGER record_shared_quota_retirements AFTER INSERT ON olp.runtime_releases FOR EACH ROW EXECUTE FUNCTION olp.record_shared_quota_retirements();

CREATE TRIGGER retained_coverage_immutable BEFORE DELETE OR UPDATE ON olp.retained_usage_coverage FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER retained_evidence_immutable BEFORE DELETE OR UPDATE ON olp.retained_usage_evidence FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER retained_liability_projection AFTER INSERT OR UPDATE OF state, templates ON olp.retained_settlements FOR EACH ROW EXECUTE FUNCTION olp.project_retained_liability();

CREATE TRIGGER retained_resource_pins BEFORE UPDATE ON olp.provider_resources FOR EACH ROW EXECUTE FUNCTION olp.guard_retained_resource_pin();

CREATE TRIGGER retained_settlement_pins BEFORE UPDATE ON olp.retained_settlements FOR EACH ROW EXECUTE FUNCTION olp.guard_retained_settlement();

CREATE TRIGGER retrieval_binding_revision_immutable BEFORE DELETE OR UPDATE ON olp.retrieval_binding_revisions FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

CREATE TRIGGER scim_members_removed AFTER DELETE ON olp.scim_group_members REFERENCING OLD TABLE AS removed_scim_members FOR EACH STATEMENT EXECUTE FUNCTION olp.scim_touch_removed_member_groups();

CREATE TRIGGER scim_user_display_changed AFTER UPDATE OF display_name, role_management ON olp.users FOR EACH ROW WHEN (((old.display_name IS DISTINCT FROM new.display_name) OR (old.role_management IS DISTINCT FROM new.role_management))) EXECUTE FUNCTION olp.scim_touch_member_groups();

CREATE TRIGGER workflow_model_nonacceptance_proof_immutable BEFORE DELETE OR UPDATE ON olp.workflow_model_nonacceptance_proofs FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();

ALTER TABLE ONLY olp.a2a_task_admissions
    ADD CONSTRAINT a2a_task_admissions_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.a2a_task_admissions
    ADD CONSTRAINT a2a_task_admissions_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.a2a_task_admissions
    ADD CONSTRAINT a2a_task_admissions_task_id_fkey FOREIGN KEY (task_id) REFERENCES olp.a2a_tasks(id);

ALTER TABLE ONLY olp.a2a_task_callback_accounting
    ADD CONSTRAINT a2a_task_callback_accounting_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.a2a_task_capabilities
    ADD CONSTRAINT a2a_task_capabilities_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.a2a_task_capabilities
    ADD CONSTRAINT a2a_task_capabilities_publication_id_fkey FOREIGN KEY (publication_id) REFERENCES olp.agent_publications(id);

ALTER TABLE ONLY olp.a2a_task_capabilities
    ADD CONSTRAINT a2a_task_capabilities_revision_id_fkey FOREIGN KEY (revision_id) REFERENCES olp.agent_publication_revisions(id);

ALTER TABLE ONLY olp.a2a_task_capabilities
    ADD CONSTRAINT a2a_task_capabilities_task_id_fkey FOREIGN KEY (task_id) REFERENCES olp.a2a_tasks(id);

ALTER TABLE ONLY olp.a2a_tasks
    ADD CONSTRAINT a2a_tasks_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.a2a_tasks
    ADD CONSTRAINT a2a_tasks_delegation_agent_id_fkey FOREIGN KEY (delegation_agent_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.a2a_tasks
    ADD CONSTRAINT a2a_tasks_delegation_member_id_fkey FOREIGN KEY (delegation_member_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.a2a_tasks
    ADD CONSTRAINT a2a_tasks_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.a2a_tasks
    ADD CONSTRAINT a2a_tasks_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.a2a_tasks
    ADD CONSTRAINT a2a_tasks_publication_id_fkey FOREIGN KEY (publication_id) REFERENCES olp.agent_publications(id);

ALTER TABLE ONLY olp.a2a_tasks
    ADD CONSTRAINT a2a_tasks_revision_id_fkey FOREIGN KEY (revision_id) REFERENCES olp.agent_publication_revisions(id);

ALTER TABLE ONLY olp.agent_approvals
    ADD CONSTRAINT agent_approvals_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.agent_approvals
    ADD CONSTRAINT agent_approvals_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.agent_approvals
    ADD CONSTRAINT agent_approvals_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.agent_approvals
    ADD CONSTRAINT agent_approvals_run_id_fkey FOREIGN KEY (run_id) REFERENCES olp.agent_runs(id);

ALTER TABLE ONLY olp.agent_credentials
    ADD CONSTRAINT agent_credentials_id_fkey FOREIGN KEY (id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.agent_credentials
    ADD CONSTRAINT agent_credentials_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.agent_delegations
    ADD CONSTRAINT agent_delegations_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.agent_delegations
    ADD CONSTRAINT agent_delegations_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.agent_delegations
    ADD CONSTRAINT agent_delegations_consenting_user_id_fkey FOREIGN KEY (consenting_user_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.agent_delegations
    ADD CONSTRAINT agent_delegations_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.agent_publication_revisions
    ADD CONSTRAINT agent_publication_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.agent_publication_revisions
    ADD CONSTRAINT agent_publication_revisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.agent_publication_revisions
    ADD CONSTRAINT agent_publication_revisions_publication_id_project_id_kind_fkey FOREIGN KEY (publication_id, project_id, kind) REFERENCES olp.agent_publications(id, project_id, kind);

ALTER TABLE ONLY olp.agent_publications
    ADD CONSTRAINT agent_publications_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.agent_publications
    ADD CONSTRAINT agent_publications_latest FOREIGN KEY (id, latest_revision_id) REFERENCES olp.agent_publication_revisions(publication_id, id);

ALTER TABLE ONLY olp.agent_publications
    ADD CONSTRAINT agent_publications_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.agent_runs
    ADD CONSTRAINT agent_runs_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.agent_runs
    ADD CONSTRAINT agent_runs_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.agent_runs
    ADD CONSTRAINT agent_runs_revision_id_fkey FOREIGN KEY (revision_id) REFERENCES olp.agent_publication_revisions(id);

ALTER TABLE ONLY olp.agent_runs
    ADD CONSTRAINT agent_runs_route_revision_id_fkey FOREIGN KEY (route_revision_id) REFERENCES olp.route_revisions(id);

ALTER TABLE ONLY olp.agent_steps
    ADD CONSTRAINT agent_steps_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.agent_steps
    ADD CONSTRAINT agent_steps_run_id_fkey FOREIGN KEY (run_id) REFERENCES olp.agent_runs(id);

ALTER TABLE ONLY olp.aggregate_budget_accounts
    ADD CONSTRAINT aggregate_budget_accounts_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.aggregate_budget_accounts
    ADD CONSTRAINT aggregate_budget_accounts_installation_id_fkey FOREIGN KEY (installation_id) REFERENCES olp.installation(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.aggregate_budget_accounts
    ADD CONSTRAINT aggregate_budget_accounts_organization_id_fkey FOREIGN KEY (organization_id) REFERENCES olp.organizations(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.aggregate_budget_accounts
    ADD CONSTRAINT aggregate_budget_accounts_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.aggregate_cost_windows
    ADD CONSTRAINT aggregate_cost_windows_account_id_fkey FOREIGN KEY (account_id) REFERENCES olp.aggregate_budget_accounts(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.api_key_cost_windows
    ADD CONSTRAINT api_key_cost_windows_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.api_key_overlaps
    ADD CONSTRAINT api_key_overlaps_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.api_keys
    ADD CONSTRAINT api_keys_budget_group_id_fkey FOREIGN KEY (budget_group_id) REFERENCES olp.budget_groups(id);

ALTER TABLE ONLY olp.api_keys
    ADD CONSTRAINT api_keys_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.api_keys
    ADD CONSTRAINT api_keys_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.api_keys
    ADD CONSTRAINT api_keys_workload_issuer_id_fkey FOREIGN KEY (workload_issuer_id) REFERENCES olp.workload_issuers(id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_agent_principal_id_fkey FOREIGN KEY (agent_principal_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_budget_group_id_fkey FOREIGN KEY (budget_group_id) REFERENCES olp.budget_groups(id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_discount_id_fkey FOREIGN KEY (discount_id) REFERENCES olp.pricing_discounts(id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_member_principal_id_fkey FOREIGN KEY (member_principal_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_pricing_revision_id_fkey FOREIGN KEY (pricing_revision_id) REFERENCES olp.pricing_revisions(id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_rate_card_revision_id_fkey FOREIGN KEY (rate_card_revision_id) REFERENCES olp.rate_card_revisions(id);

ALTER TABLE ONLY olp.attempt_usage_facts
    ADD CONSTRAINT attempt_usage_facts_request_id_request_started_at_fkey FOREIGN KEY (request_id, request_started_at) REFERENCES olp.usage_request_anchors(request_id, request_started_at) ON DELETE CASCADE;

ALTER TABLE ONLY olp.attempt_usage_hourly
    ADD CONSTRAINT attempt_usage_hourly_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.attempt_usage_hourly
    ADD CONSTRAINT attempt_usage_hourly_budget_group_id_fkey FOREIGN KEY (budget_group_id) REFERENCES olp.budget_groups(id);

ALTER TABLE ONLY olp.attempt_usage_hourly
    ADD CONSTRAINT attempt_usage_hourly_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id);

ALTER TABLE ONLY olp.attempts
    ADD CONSTRAINT attempts_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id);

ALTER TABLE ONLY olp.attempts
    ADD CONSTRAINT attempts_request_id_request_started_at_fkey FOREIGN KEY (request_id, request_started_at) REFERENCES olp.requests(id, started_at) ON DELETE CASCADE;

ALTER TABLE ONLY olp.audit
    ADD CONSTRAINT audit_actor_management_token_id_fkey FOREIGN KEY (actor_management_token_id) REFERENCES olp.management_tokens(id);

ALTER TABLE ONLY olp.audit
    ADD CONSTRAINT audit_actor_user_id_fkey FOREIGN KEY (actor_user_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.batch_pending_recorded_owners
    ADD CONSTRAINT batch_pending_recorded_owners_resource_id_ordinal_fkey FOREIGN KEY (resource_id, ordinal) REFERENCES olp.batch_pending_subjects(resource_id, ordinal) ON DELETE CASCADE;

ALTER TABLE ONLY olp.batch_pending_subjects
    ADD CONSTRAINT batch_pending_subjects_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES olp.batch_settlements(resource_id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.batch_settlement_results
    ADD CONSTRAINT batch_settlement_results_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES olp.batch_settlements(resource_id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.notification_deliveries
    ADD CONSTRAINT budget_alert_deliveries_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES olp.notification_rules(id);

ALTER TABLE ONLY olp.notification_rules
    ADD CONSTRAINT budget_alert_rules_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.notification_rules
    ADD CONSTRAINT budget_alert_rules_destination_id_fkey FOREIGN KEY (destination_id) REFERENCES olp.notification_destinations(id);

ALTER TABLE ONLY olp.notification_rules
    ADD CONSTRAINT budget_alert_rules_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.budget_group_cost_windows
    ADD CONSTRAINT budget_group_cost_windows_budget_group_id_fkey FOREIGN KEY (budget_group_id) REFERENCES olp.budget_groups(id);

ALTER TABLE ONLY olp.budget_groups
    ADD CONSTRAINT budget_groups_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.budget_groups
    ADD CONSTRAINT budget_groups_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.budget_increase_requests
    ADD CONSTRAINT budget_increase_requests_decided_by_fkey FOREIGN KEY (decided_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.budget_increase_requests
    ADD CONSTRAINT budget_increase_requests_increase_id_fkey FOREIGN KEY (increase_id) REFERENCES olp.budget_increases(id);

ALTER TABLE ONLY olp.budget_increase_requests
    ADD CONSTRAINT budget_increase_requests_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.budget_increase_requests
    ADD CONSTRAINT budget_increase_requests_requested_by_fkey FOREIGN KEY (requested_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.budget_increases
    ADD CONSTRAINT budget_increases_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.budget_increases
    ADD CONSTRAINT budget_increases_organization_id_fkey FOREIGN KEY (organization_id) REFERENCES olp.organizations(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.budget_increases
    ADD CONSTRAINT budget_increases_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.budget_increases
    ADD CONSTRAINT budget_increases_revoked_by_fkey FOREIGN KEY (revoked_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.capture_configuration
    ADD CONSTRAINT capture_configuration_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.capture_policies
    ADD CONSTRAINT capture_policies_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.capture_policies
    ADD CONSTRAINT capture_policies_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.capture_policies
    ADD CONSTRAINT capture_policies_route_slug_fkey FOREIGN KEY (route_slug) REFERENCES olp.routes(slug);

ALTER TABLE ONLY olp.capture_policies
    ADD CONSTRAINT capture_policies_sink_id_fkey FOREIGN KEY (sink_id) REFERENCES olp.export_sinks(id);

ALTER TABLE ONLY olp.capture_redactors
    ADD CONSTRAINT capture_redactors_policy_id_fkey FOREIGN KEY (policy_id) REFERENCES olp.capture_policies(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.capture_redactors
    ADD CONSTRAINT capture_redactors_revision_id_fkey FOREIGN KEY (revision_id) REFERENCES olp.guardrail_revisions(id);

ALTER TABLE ONLY olp.code_account_cooldowns
    ADD CONSTRAINT code_account_cooldowns_account_id_fkey FOREIGN KEY (account_id) REFERENCES olp.code_accounts(id);

ALTER TABLE ONLY olp.code_accounts
    ADD CONSTRAINT code_accounts_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.code_accounts
    ADD CONSTRAINT code_accounts_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.provider_credentials(id);

ALTER TABLE ONLY olp.code_accounts
    ADD CONSTRAINT code_accounts_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.code_accounts
    ADD CONSTRAINT code_accounts_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_account_id_fkey FOREIGN KEY (account_id) REFERENCES olp.code_accounts(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES olp.code_bindings(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_paid_binding_id_fkey FOREIGN KEY (paid_binding_id) REFERENCES olp.code_paid_bindings(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_paid_provider_id_fkey FOREIGN KEY (paid_provider_id) REFERENCES olp.providers(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_paid_slot_id_fkey FOREIGN KEY (paid_slot_id) REFERENCES olp.provider_slots(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_recovery_from_fkey FOREIGN KEY (recovery_from) REFERENCES olp.code_attempts(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.code_routes(id);

ALTER TABLE ONLY olp.code_attempts
    ADD CONSTRAINT code_attempts_route_revision_id_fkey FOREIGN KEY (route_revision_id) REFERENCES olp.code_route_revisions(id);

ALTER TABLE ONLY olp.code_bindings
    ADD CONSTRAINT code_bindings_account_id_fkey FOREIGN KEY (account_id) REFERENCES olp.code_accounts(id);

ALTER TABLE ONLY olp.code_bindings
    ADD CONSTRAINT code_bindings_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_bindings
    ADD CONSTRAINT code_bindings_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES olp.code_bindings(id);

ALTER TABLE ONLY olp.code_bindings
    ADD CONSTRAINT code_bindings_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.code_bindings
    ADD CONSTRAINT code_bindings_root_id_fkey FOREIGN KEY (root_id) REFERENCES olp.code_bindings(id);

ALTER TABLE ONLY olp.code_bindings
    ADD CONSTRAINT code_bindings_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.code_routes(id);

ALTER TABLE ONLY olp.code_decisions
    ADD CONSTRAINT code_decisions_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_decisions
    ADD CONSTRAINT code_decisions_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES olp.code_attempts(id);

ALTER TABLE ONLY olp.code_decisions
    ADD CONSTRAINT code_decisions_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES olp.code_bindings(id);

ALTER TABLE ONLY olp.code_decisions
    ADD CONSTRAINT code_decisions_chosen_account_id_fkey FOREIGN KEY (chosen_account_id) REFERENCES olp.code_accounts(id);

ALTER TABLE ONLY olp.code_decisions
    ADD CONSTRAINT code_decisions_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.code_decisions
    ADD CONSTRAINT code_decisions_root_id_fkey FOREIGN KEY (root_id) REFERENCES olp.code_bindings(id);

ALTER TABLE ONLY olp.code_decisions
    ADD CONSTRAINT code_decisions_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.code_routes(id);

ALTER TABLE ONLY olp.code_decisions
    ADD CONSTRAINT code_decisions_route_revision_id_fkey FOREIGN KEY (route_revision_id) REFERENCES olp.code_route_revisions(id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.provider_credentials(id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_parent_id_fkey FOREIGN KEY (parent_id) REFERENCES olp.code_paid_bindings(id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_root_id_fkey FOREIGN KEY (root_id) REFERENCES olp.code_paid_bindings(id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.code_routes(id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_slot_id_fkey FOREIGN KEY (slot_id) REFERENCES olp.provider_slots(id);

ALTER TABLE ONLY olp.code_paid_bindings
    ADD CONSTRAINT code_paid_bindings_subscription_root_id_fkey FOREIGN KEY (subscription_root_id) REFERENCES olp.code_bindings(id);

ALTER TABLE ONLY olp.code_paid_references
    ADD CONSTRAINT code_paid_references_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_paid_references
    ADD CONSTRAINT code_paid_references_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES olp.code_paid_bindings(id);

ALTER TABLE ONLY olp.code_paid_references
    ADD CONSTRAINT code_paid_references_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.code_routes(id);

ALTER TABLE ONLY olp.code_pins
    ADD CONSTRAINT code_pins_account_id_fkey FOREIGN KEY (account_id) REFERENCES olp.code_accounts(id);

ALTER TABLE ONLY olp.code_pins
    ADD CONSTRAINT code_pins_root_id_fkey FOREIGN KEY (root_id) REFERENCES olp.code_bindings(id);

ALTER TABLE ONLY olp.code_pool_accounts
    ADD CONSTRAINT code_pool_accounts_account_id_fkey FOREIGN KEY (account_id) REFERENCES olp.code_accounts(id);

ALTER TABLE ONLY olp.code_pool_accounts
    ADD CONSTRAINT code_pool_accounts_pool_id_fkey FOREIGN KEY (pool_id) REFERENCES olp.code_pools(id);

ALTER TABLE ONLY olp.code_pool_keys
    ADD CONSTRAINT code_pool_keys_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_pool_keys
    ADD CONSTRAINT code_pool_keys_pool_id_fkey FOREIGN KEY (pool_id) REFERENCES olp.code_pools(id);

ALTER TABLE ONLY olp.code_pools
    ADD CONSTRAINT code_pools_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.code_pools
    ADD CONSTRAINT code_pools_owner_user_id_fkey FOREIGN KEY (owner_user_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.code_pools
    ADD CONSTRAINT code_pools_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.code_references
    ADD CONSTRAINT code_references_account_id_fkey FOREIGN KEY (account_id) REFERENCES olp.code_accounts(id);

ALTER TABLE ONLY olp.code_references
    ADD CONSTRAINT code_references_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_references
    ADD CONSTRAINT code_references_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES olp.code_bindings(id);

ALTER TABLE ONLY olp.code_references
    ADD CONSTRAINT code_references_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.code_routes(id);

ALTER TABLE ONLY olp.code_refusals
    ADD CONSTRAINT code_refusals_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_refusals
    ADD CONSTRAINT code_refusals_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.code_refusals
    ADD CONSTRAINT code_refusals_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.code_routes(id);

ALTER TABLE ONLY olp.code_research_sessions
    ADD CONSTRAINT code_research_sessions_account_id_fkey FOREIGN KEY (account_id) REFERENCES olp.code_accounts(id);

ALTER TABLE ONLY olp.code_research_sessions
    ADD CONSTRAINT code_research_sessions_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_research_sessions
    ADD CONSTRAINT code_research_sessions_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.provider_credentials(id);

ALTER TABLE ONLY olp.code_research_sessions
    ADD CONSTRAINT code_research_sessions_revision_id_fkey FOREIGN KEY (revision_id) REFERENCES olp.code_route_revisions(id);

ALTER TABLE ONLY olp.code_route_revisions
    ADD CONSTRAINT code_route_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.code_route_revisions
    ADD CONSTRAINT code_route_revisions_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.code_routes(id);

ALTER TABLE ONLY olp.code_routes
    ADD CONSTRAINT code_routes_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.code_routes
    ADD CONSTRAINT code_routes_latest_revision_id_fkey FOREIGN KEY (latest_revision_id) REFERENCES olp.code_route_revisions(id);

ALTER TABLE ONLY olp.code_routes
    ADD CONSTRAINT code_routes_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.code_token_budgets
    ADD CONSTRAINT code_token_budgets_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.code_token_budgets
    ADD CONSTRAINT code_token_budgets_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.code_token_budgets
    ADD CONSTRAINT code_token_budgets_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.code_token_budgets
    ADD CONSTRAINT code_token_budgets_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.code_routes(id);

ALTER TABLE ONLY olp.code_token_reservations
    ADD CONSTRAINT code_token_reservations_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES olp.code_attempts(id);

ALTER TABLE ONLY olp.code_token_reservations
    ADD CONSTRAINT code_token_reservations_budget_id_period_starts_at_fkey FOREIGN KEY (budget_id, period, starts_at) REFERENCES olp.code_token_windows(budget_id, period, starts_at);

ALTER TABLE ONLY olp.code_token_windows
    ADD CONSTRAINT code_token_windows_budget_id_fkey FOREIGN KEY (budget_id) REFERENCES olp.code_token_budgets(id);

ALTER TABLE ONLY olp.commitment_periods
    ADD CONSTRAINT commitment_periods_schedule_id_fkey FOREIGN KEY (schedule_id) REFERENCES olp.commitment_schedules(id);

ALTER TABLE ONLY olp.commitment_usage
    ADD CONSTRAINT commitment_usage_period_id_fkey FOREIGN KEY (period_id) REFERENCES olp.commitment_periods(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.end_user_accounts
    ADD CONSTRAINT end_user_accounts_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.end_user_accounts
    ADD CONSTRAINT end_user_accounts_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.end_user_cost_windows
    ADD CONSTRAINT end_user_cost_windows_account_id_fkey FOREIGN KEY (account_id) REFERENCES olp.end_user_accounts(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.execution_debit_owners
    ADD CONSTRAINT execution_debit_owners_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.execution_debit_resolutions
    ADD CONSTRAINT execution_debit_resolutions_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.execution_debit_resolutions
    ADD CONSTRAINT execution_debit_resolutions_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.execution_debit_result_resolutions
    ADD CONSTRAINT execution_debit_result_resolu_dispatch_id_evidence_result__fkey FOREIGN KEY (dispatch_id, evidence_result_digest) REFERENCES olp.execution_debit_results(dispatch_id, result_digest);

ALTER TABLE ONLY olp.execution_debit_result_resolutions
    ADD CONSTRAINT execution_debit_result_resolut_dispatch_id_resolved_digest_fkey FOREIGN KEY (dispatch_id, resolved_digest) REFERENCES olp.execution_debit_results(dispatch_id, result_digest);

ALTER TABLE ONLY olp.execution_debit_results
    ADD CONSTRAINT execution_debit_results_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.execution_debits
    ADD CONSTRAINT execution_debits_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.execution_debits
    ADD CONSTRAINT execution_debits_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.execution_debits
    ADD CONSTRAINT execution_debits_run FOREIGN KEY (run_id) REFERENCES olp.agent_runs(id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp.execution_debits
    ADD CONSTRAINT execution_debits_step FOREIGN KEY (run_id, step_id) REFERENCES olp.agent_steps(run_id, id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp.export_cursors
    ADD CONSTRAINT export_cursors_sink_id_fkey FOREIGN KEY (sink_id) REFERENCES olp.export_sinks(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.export_gaps
    ADD CONSTRAINT export_gaps_sink_id_fkey FOREIGN KEY (sink_id) REFERENCES olp.export_sinks(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.export_pending
    ADD CONSTRAINT export_pending_record_id_fkey FOREIGN KEY (record_id) REFERENCES olp.export_records(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.export_pending
    ADD CONSTRAINT export_pending_sink_id_fkey FOREIGN KEY (sink_id) REFERENCES olp.export_sinks(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.export_sinks
    ADD CONSTRAINT export_sinks_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.export_sinks
    ADD CONSTRAINT export_sinks_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.export_sinks
    ADD CONSTRAINT export_sinks_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.focus_datasets
    ADD CONSTRAINT focus_datasets_sink_id_fkey FOREIGN KEY (sink_id) REFERENCES olp.export_sinks(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.grant_enrollments
    ADD CONSTRAINT grant_enrollments_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.provider_credentials(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.grant_enrollments
    ADD CONSTRAINT grant_enrollments_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.grant_enrollments
    ADD CONSTRAINT grant_enrollments_slot_id_fkey FOREIGN KEY (slot_id) REFERENCES olp.provider_slots(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.guardrails
    ADD CONSTRAINT guardrail_active_revision_owner FOREIGN KEY (active_revision_id, id) REFERENCES olp.guardrail_revisions(id, guardrail_id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp.guardrail_attachments
    ADD CONSTRAINT guardrail_attachments_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.guardrail_attachments
    ADD CONSTRAINT guardrail_attachments_policy_revision_id_fkey FOREIGN KEY (policy_revision_id) REFERENCES olp.guardrail_policy_revisions(id);

ALTER TABLE ONLY olp.guardrail_credentials
    ADD CONSTRAINT guardrail_credentials_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.guardrail_credentials
    ADD CONSTRAINT guardrail_credentials_id_fkey FOREIGN KEY (id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.guardrail_credentials
    ADD CONSTRAINT guardrail_credentials_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.guardrail_draft_attachments
    ADD CONSTRAINT guardrail_draft_attachments_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.guardrail_draft_attachments
    ADD CONSTRAINT guardrail_draft_attachments_draft_id_fkey FOREIGN KEY (draft_id) REFERENCES olp.route_drafts(id);

ALTER TABLE ONLY olp.guardrail_draft_attachments
    ADD CONSTRAINT guardrail_draft_attachments_policy_revision_id_fkey FOREIGN KEY (policy_revision_id) REFERENCES olp.guardrail_policy_revisions(id);

ALTER TABLE ONLY olp.guardrails
    ADD CONSTRAINT guardrail_latest_revision_owner FOREIGN KEY (latest_revision_id, id) REFERENCES olp.guardrail_revisions(id, guardrail_id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp.guardrail_policies
    ADD CONSTRAINT guardrail_policies_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.guardrail_policies
    ADD CONSTRAINT guardrail_policies_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.guardrail_policies
    ADD CONSTRAINT guardrail_policy_active_owner FOREIGN KEY (active_revision_id, id) REFERENCES olp.guardrail_policy_revisions(id, policy_id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp.guardrail_policies
    ADD CONSTRAINT guardrail_policy_latest_owner FOREIGN KEY (latest_revision_id, id) REFERENCES olp.guardrail_policy_revisions(id, policy_id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp.guardrail_policy_revision_activations
    ADD CONSTRAINT guardrail_policy_revision_activations_activated_by_fkey FOREIGN KEY (activated_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.guardrail_policy_revision_activations
    ADD CONSTRAINT guardrail_policy_revision_activations_revision_id_fkey FOREIGN KEY (revision_id) REFERENCES olp.guardrail_policy_revisions(id);

ALTER TABLE ONLY olp.guardrail_policy_revisions
    ADD CONSTRAINT guardrail_policy_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.guardrail_policy_revisions
    ADD CONSTRAINT guardrail_policy_revisions_policy_id_fkey FOREIGN KEY (policy_id) REFERENCES olp.guardrail_policies(id);

ALTER TABLE ONLY olp.guardrail_revision_activations
    ADD CONSTRAINT guardrail_revision_activations_activated_by_fkey FOREIGN KEY (activated_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.guardrail_revision_activations
    ADD CONSTRAINT guardrail_revision_activations_revision_id_fkey FOREIGN KEY (revision_id) REFERENCES olp.guardrail_revisions(id);

ALTER TABLE ONLY olp.guardrail_revisions
    ADD CONSTRAINT guardrail_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.guardrail_revisions
    ADD CONSTRAINT guardrail_revisions_guardrail_id_fkey FOREIGN KEY (guardrail_id) REFERENCES olp.guardrails(id);

ALTER TABLE ONLY olp.guardrails
    ADD CONSTRAINT guardrails_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.guardrails
    ADD CONSTRAINT guardrails_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.investigations
    ADD CONSTRAINT investigations_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.invitations
    ADD CONSTRAINT invitations_accepted_by_fkey FOREIGN KEY (accepted_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.invitations
    ADD CONSTRAINT invitations_invited_by_fkey FOREIGN KEY (invited_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.invitations
    ADD CONSTRAINT invitations_revoked_by_fkey FOREIGN KEY (revoked_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.managed_export_deliveries
    ADD CONSTRAINT managed_export_deliveries_sink_id_fkey FOREIGN KEY (sink_id) REFERENCES olp.managed_export_sinks(id);

ALTER TABLE ONLY olp.managed_export_sinks
    ADD CONSTRAINT managed_export_sinks_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.managed_export_sinks
    ADD CONSTRAINT managed_export_sinks_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.managed_export_sinks
    ADD CONSTRAINT managed_export_sinks_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.management_tokens
    ADD CONSTRAINT management_tokens_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.mcp_call_identities
    ADD CONSTRAINT mcp_call_identities_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.mcp_call_identities
    ADD CONSTRAINT mcp_call_identities_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.mcp_call_identities
    ADD CONSTRAINT mcp_call_identities_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.mcp_call_identities
    ADD CONSTRAINT mcp_call_identities_toolset_id_fkey FOREIGN KEY (toolset_id) REFERENCES olp.agent_publications(id);

ALTER TABLE ONLY olp.mcp_call_identities
    ADD CONSTRAINT mcp_call_identities_toolset_revision_id_fkey FOREIGN KEY (toolset_revision_id) REFERENCES olp.agent_publication_revisions(id);

ALTER TABLE ONLY olp.mcp_executable_permissions
    ADD CONSTRAINT mcp_executable_permissions_approved_by_fkey FOREIGN KEY (approved_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.mcp_grant_enrollments
    ADD CONSTRAINT mcp_grant_enrollments_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES olp.agent_publications(id);

ALTER TABLE ONLY olp.mcp_grant_enrollments
    ADD CONSTRAINT mcp_grant_enrollments_claimed_by_fkey FOREIGN KEY (claimed_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.mcp_grant_enrollments
    ADD CONSTRAINT mcp_grant_enrollments_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.mcp_grant_enrollments
    ADD CONSTRAINT mcp_grant_enrollments_server_id_fkey FOREIGN KEY (server_id) REFERENCES olp.mcp_servers(id);

ALTER TABLE ONLY olp.mcp_grant_enrollments
    ADD CONSTRAINT mcp_grant_enrollments_session_secret_id_fkey FOREIGN KEY (session_secret_id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.mcp_grant_enrollments
    ADD CONSTRAINT mcp_grant_enrollments_started_by_fkey FOREIGN KEY (started_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.mcp_servers
    ADD CONSTRAINT mcp_latest_revision_owner FOREIGN KEY (latest_revision_id, id) REFERENCES olp.mcp_server_revisions(id, server_id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp.mcp_server_revisions
    ADD CONSTRAINT mcp_server_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.mcp_server_revisions
    ADD CONSTRAINT mcp_server_revisions_server_id_fkey FOREIGN KEY (server_id) REFERENCES olp.mcp_servers(id);

ALTER TABLE ONLY olp.mcp_servers
    ADD CONSTRAINT mcp_servers_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.mcp_servers
    ADD CONSTRAINT mcp_servers_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.mcp_servers
    ADD CONSTRAINT mcp_servers_delegation_key_id_fkey FOREIGN KEY (delegation_key_id) REFERENCES olp.secrets(id) ON DELETE SET NULL;

ALTER TABLE ONLY olp.mcp_servers
    ADD CONSTRAINT mcp_servers_entra_client_secret_id_fkey FOREIGN KEY (entra_client_secret_id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.mcp_servers
    ADD CONSTRAINT mcp_servers_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_agent_principal_id_fkey FOREIGN KEY (agent_principal_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_member_principal_id_fkey FOREIGN KEY (member_principal_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_server_id_fkey FOREIGN KEY (server_id) REFERENCES olp.mcp_servers(id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_server_revision_id_fkey FOREIGN KEY (server_revision_id) REFERENCES olp.mcp_server_revisions(id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_toolset_id_fkey FOREIGN KEY (toolset_id) REFERENCES olp.agent_publications(id);

ALTER TABLE ONLY olp.mcp_tool_calls
    ADD CONSTRAINT mcp_tool_calls_toolset_revision_id_fkey FOREIGN KEY (toolset_revision_id) REFERENCES olp.agent_publication_revisions(id);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_access_secret_id_fkey FOREIGN KEY (access_secret_id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_agent_id_fkey FOREIGN KEY (agent_id) REFERENCES olp.agent_publications(id);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_enrollment_id_fkey FOREIGN KEY (enrollment_id) REFERENCES olp.mcp_grant_enrollments(id);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_member_id_fkey FOREIGN KEY (member_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_refresh_secret_id_fkey FOREIGN KEY (refresh_secret_id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.mcp_user_grants
    ADD CONSTRAINT mcp_user_grants_server_id_fkey FOREIGN KEY (server_id) REFERENCES olp.mcp_servers(id);

ALTER TABLE ONLY olp.media_jobs
    ADD CONSTRAINT media_jobs_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.media_jobs
    ADD CONSTRAINT media_jobs_credential_version_id_fkey FOREIGN KEY (credential_version_id) REFERENCES olp.provider_credentials(id);

ALTER TABLE ONLY olp.media_jobs
    ADD CONSTRAINT media_jobs_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id);

ALTER TABLE ONLY olp.media_jobs
    ADD CONSTRAINT media_jobs_provider_revision_id_fkey FOREIGN KEY (provider_revision_id) REFERENCES olp.provider_revisions(id);

ALTER TABLE ONLY olp.media_jobs
    ADD CONSTRAINT media_jobs_runtime_generation_id_fkey FOREIGN KEY (runtime_generation_id) REFERENCES olp.runtime_releases(id);

ALTER TABLE ONLY olp.metering_delivery_receipts
    ADD CONSTRAINT metering_delivery_receipts_sink_id_fkey FOREIGN KEY (sink_id) REFERENCES olp.export_sinks(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.mfa_challenges
    ADD CONSTRAINT mfa_challenges_secret_id_fkey FOREIGN KEY (secret_id) REFERENCES olp.secrets(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.mfa_challenges
    ADD CONSTRAINT mfa_challenges_session_id_fkey FOREIGN KEY (session_id) REFERENCES olp.sessions(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.mfa_challenges
    ADD CONSTRAINT mfa_challenges_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.mfa_factors
    ADD CONSTRAINT mfa_factors_id_fkey FOREIGN KEY (id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.mfa_factors
    ADD CONSTRAINT mfa_factors_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.mfa_recovery_codes
    ADD CONSTRAINT mfa_recovery_codes_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.model_catalog_project_settings
    ADD CONSTRAINT model_catalog_project_settings_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.model_catalog_route_settings
    ADD CONSTRAINT model_catalog_route_settings_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.routes(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.notification_deliveries
    ADD CONSTRAINT notification_deliveries_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.notification_deliveries
    ADD CONSTRAINT notification_deliveries_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.provider_credentials(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.notification_destination_tests
    ADD CONSTRAINT notification_destination_tests_destination_id_fkey FOREIGN KEY (destination_id) REFERENCES olp.notification_destinations(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.notification_destinations
    ADD CONSTRAINT notification_destinations_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.notification_destinations
    ADD CONSTRAINT notification_destinations_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.notification_destinations
    ADD CONSTRAINT notification_destinations_secret_id_fkey FOREIGN KEY (secret_id) REFERENCES olp.secrets(id) ON DELETE SET NULL;

ALTER TABLE ONLY olp.notification_signal_states
    ADD CONSTRAINT notification_signal_states_rule_id_fkey FOREIGN KEY (rule_id) REFERENCES olp.notification_rules(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.oidc_configuration
    ADD CONSTRAINT oidc_configuration_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.oidc_flows
    ADD CONSTRAINT oidc_flows_id_fkey FOREIGN KEY (id) REFERENCES olp.secrets(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.oidc_identities
    ADD CONSTRAINT oidc_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.organization_members
    ADD CONSTRAINT organization_members_added_by_fkey FOREIGN KEY (added_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.organization_members
    ADD CONSTRAINT organization_members_organization_id_fkey FOREIGN KEY (organization_id) REFERENCES olp.organizations(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.organization_members
    ADD CONSTRAINT organization_members_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.organizations
    ADD CONSTRAINT organizations_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.pending_resource_recorded_owners
    ADD CONSTRAINT pending_resource_recorded_owners_resource_id_ordinal_fkey FOREIGN KEY (resource_id, ordinal) REFERENCES olp.provider_resource_pending_usage(resource_id, ordinal) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_resource_pending_usage
    ADD CONSTRAINT pending_usage_original_group FOREIGN KEY (budget_group_id) REFERENCES olp.budget_groups(id);

ALTER TABLE ONLY olp.plugins
    ADD CONSTRAINT plugins_approved_by_fkey FOREIGN KEY (approved_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.plugins
    ADD CONSTRAINT plugins_installed_by_fkey FOREIGN KEY (installed_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.prices
    ADD CONSTRAINT prices_pricing_revision_id_fkey FOREIGN KEY (pricing_revision_id) REFERENCES olp.pricing_revisions(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.prices
    ADD CONSTRAINT prices_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.pricing_discounts
    ADD CONSTRAINT pricing_discounts_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.pricing_discounts
    ADD CONSTRAINT pricing_discounts_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.pricing_revisions
    ADD CONSTRAINT pricing_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.pricing_revisions
    ADD CONSTRAINT pricing_revisions_source_snapshot_id_fkey FOREIGN KEY (source_snapshot_id) REFERENCES olp.pricing_source_snapshots(id);

ALTER TABLE ONLY olp.pricing_source_snapshots
    ADD CONSTRAINT pricing_source_snapshots_source_id_fkey FOREIGN KEY (source_id) REFERENCES olp.pricing_sources(id);

ALTER TABLE ONLY olp.pricing_sources
    ADD CONSTRAINT pricing_sources_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.project_members
    ADD CONSTRAINT project_members_added_by_fkey FOREIGN KEY (added_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.project_members
    ADD CONSTRAINT project_members_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.project_members
    ADD CONSTRAINT project_members_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.projects
    ADD CONSTRAINT projects_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.projects
    ADD CONSTRAINT projects_organization_id_fkey FOREIGN KEY (organization_id) REFERENCES olp.organizations(id);

ALTER TABLE ONLY olp.prompt_labels
    ADD CONSTRAINT prompt_labels_publication_id_fkey FOREIGN KEY (publication_id) REFERENCES olp.agent_publications(id);

ALTER TABLE ONLY olp.prompt_labels
    ADD CONSTRAINT prompt_labels_publication_id_revision_id_fkey FOREIGN KEY (publication_id, revision_id) REFERENCES olp.agent_publication_revisions(publication_id, id);

ALTER TABLE ONLY olp.provider_billing_contexts
    ADD CONSTRAINT provider_billing_contexts_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_billing_days
    ADD CONSTRAINT provider_billing_days_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_billing_imports
    ADD CONSTRAINT provider_billing_imports_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.provider_billing_imports
    ADD CONSTRAINT provider_billing_imports_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.provider_billing_imports
    ADD CONSTRAINT provider_billing_imports_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_billing_revisions
    ADD CONSTRAINT provider_billing_revisions_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_credentials
    ADD CONSTRAINT provider_credentials_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_grants
    ADD CONSTRAINT provider_grants_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.provider_credentials(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_grants
    ADD CONSTRAINT provider_grants_refresh_token_id_fkey FOREIGN KEY (refresh_token_id) REFERENCES olp.secrets(id);

ALTER TABLE ONLY olp.provider_invoiced_costs
    ADD CONSTRAINT provider_invoiced_costs_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_models
    ADD CONSTRAINT provider_models_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_network_credentials
    ADD CONSTRAINT provider_network_credentials_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_resource_pending_usage
    ADD CONSTRAINT provider_resource_pending_usage_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES olp.provider_resources(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_resources
    ADD CONSTRAINT provider_resources_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.provider_resources
    ADD CONSTRAINT provider_resources_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.provider_credentials(id);

ALTER TABLE ONLY olp.provider_resources
    ADD CONSTRAINT provider_resources_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id);

ALTER TABLE ONLY olp.provider_revisions
    ADD CONSTRAINT provider_revisions_activated_by_fkey FOREIGN KEY (activated_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.provider_revisions
    ADD CONSTRAINT provider_revisions_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.provider_slots
    ADD CONSTRAINT provider_slots_credential_id_fkey FOREIGN KEY (credential_id) REFERENCES olp.provider_credentials(id);

ALTER TABLE ONLY olp.provider_slots
    ADD CONSTRAINT provider_slots_provider_id_fkey FOREIGN KEY (provider_id) REFERENCES olp.providers(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.providers
    ADD CONSTRAINT providers_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.providers
    ADD CONSTRAINT providers_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.provisioned_users
    ADD CONSTRAINT provisioned_users_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.rate_card_prices
    ADD CONSTRAINT rate_card_prices_rate_card_revision_id_fkey FOREIGN KEY (rate_card_revision_id) REFERENCES olp.rate_card_revisions(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.rate_card_revisions
    ADD CONSTRAINT rate_card_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.rate_card_revisions
    ADD CONSTRAINT rate_card_revisions_rate_card_id_fkey FOREIGN KEY (rate_card_id) REFERENCES olp.rate_cards(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.rate_cards
    ADD CONSTRAINT rate_cards_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.rate_cards
    ADD CONSTRAINT rate_cards_organization_id_fkey FOREIGN KEY (organization_id) REFERENCES olp.organizations(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.rate_cards
    ADD CONSTRAINT rate_cards_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.recent_auth
    ADD CONSTRAINT recent_auth_session_id_fkey FOREIGN KEY (session_id) REFERENCES olp.sessions(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.replays
    ADD CONSTRAINT replays_secret_id_fkey FOREIGN KEY (secret_id) REFERENCES olp.secrets(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.request_metadata_gateway_epochs
    ADD CONSTRAINT request_metadata_gateway_epochs_acknowledged_by_fkey FOREIGN KEY (acknowledged_by) REFERENCES olp.users(id) ON DELETE SET NULL;

ALTER TABLE ONLY olp.request_metadata_gateway_epochs
    ADD CONSTRAINT request_metadata_gateway_epochs_uncertainty_gap_id_fkey FOREIGN KEY (uncertainty_gap_id) REFERENCES olp.request_metadata_ingestion_gaps(id) ON DELETE SET NULL;

ALTER TABLE olp.requests
    ADD CONSTRAINT requests_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE olp.requests
    ADD CONSTRAINT requests_budget_group_id_fkey FOREIGN KEY (budget_group_id) REFERENCES olp.budget_groups(id);

ALTER TABLE ONLY olp.retained_mutations
    ADD CONSTRAINT retained_mutations_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.retained_mutations
    ADD CONSTRAINT retained_mutations_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES olp.provider_resources(id);

ALTER TABLE ONLY olp.retained_settlements
    ADD CONSTRAINT retained_settlements_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES olp.provider_resources(id);

ALTER TABLE ONLY olp.retained_submissions
    ADD CONSTRAINT retained_submissions_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.retained_submissions
    ADD CONSTRAINT retained_submissions_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES olp.provider_resources(id);

ALTER TABLE ONLY olp.retained_usage_coverage
    ADD CONSTRAINT retained_usage_coverage_evidence_id_fkey FOREIGN KEY (evidence_id) REFERENCES olp.retained_usage_evidence(id);

ALTER TABLE ONLY olp.retained_usage_coverage
    ADD CONSTRAINT retained_usage_coverage_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES olp.retained_settlements(resource_id);

ALTER TABLE ONLY olp.retained_usage_evidence
    ADD CONSTRAINT retained_usage_evidence_resource_id_fkey FOREIGN KEY (resource_id) REFERENCES olp.retained_settlements(resource_id);

ALTER TABLE ONLY olp.retrieval_bindings
    ADD CONSTRAINT retrieval_binding_latest FOREIGN KEY (latest_revision_id) REFERENCES olp.retrieval_binding_revisions(id) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE ONLY olp.retrieval_binding_revisions
    ADD CONSTRAINT retrieval_binding_revisions_binding_id_fkey FOREIGN KEY (binding_id) REFERENCES olp.retrieval_bindings(id);

ALTER TABLE ONLY olp.retrieval_binding_revisions
    ADD CONSTRAINT retrieval_binding_revisions_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.retrieval_bindings
    ADD CONSTRAINT retrieval_bindings_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.retrieval_bindings
    ADD CONSTRAINT retrieval_bindings_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.route_drafts
    ADD CONSTRAINT route_drafts_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.route_drafts
    ADD CONSTRAINT route_drafts_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.route_revisions
    ADD CONSTRAINT route_revisions_activated_by_fkey FOREIGN KEY (activated_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.route_revisions
    ADD CONSTRAINT route_revisions_route_id_fkey FOREIGN KEY (route_id) REFERENCES olp.routes(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.route_template_routes
    ADD CONSTRAINT route_template_routes_draft_id_fkey FOREIGN KEY (draft_id) REFERENCES olp.route_drafts(id) ON DELETE SET NULL;

ALTER TABLE ONLY olp.route_template_routes
    ADD CONSTRAINT route_template_routes_template_id_fkey FOREIGN KEY (template_id) REFERENCES olp.route_templates(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.route_templates
    ADD CONSTRAINT route_templates_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.route_templates
    ADD CONSTRAINT route_templates_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.routes
    ADD CONSTRAINT routes_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.routes
    ADD CONSTRAINT routes_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.routes
    ADD CONSTRAINT routes_retired_by_fkey FOREIGN KEY (retired_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.routing_policies
    ADD CONSTRAINT routing_policies_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.runtime_releases
    ADD CONSTRAINT runtime_releases_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.runtime_shared_quota_retirements
    ADD CONSTRAINT runtime_shared_quota_retirements_sequence_fkey FOREIGN KEY (sequence) REFERENCES olp.runtime_releases(sequence) ON DELETE CASCADE;

ALTER TABLE ONLY olp.saml_configuration
    ADD CONSTRAINT saml_configuration_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.saml_flows
    ADD CONSTRAINT saml_flows_id_fkey FOREIGN KEY (id) REFERENCES olp.secrets(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.saml_identities
    ADD CONSTRAINT saml_identities_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.scim_group_members
    ADD CONSTRAINT scim_group_members_group_id_fkey FOREIGN KEY (group_id) REFERENCES olp.scim_groups(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.scim_group_members
    ADD CONSTRAINT scim_group_members_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.scim_users(user_id);

ALTER TABLE ONLY olp.scim_group_projects
    ADD CONSTRAINT scim_group_projects_group_id_fkey FOREIGN KEY (group_id) REFERENCES olp.scim_groups(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.scim_group_projects
    ADD CONSTRAINT scim_group_projects_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.scim_users
    ADD CONSTRAINT scim_users_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.sessions
    ADD CONSTRAINT sessions_user_id_fkey FOREIGN KEY (user_id) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.settings
    ADD CONSTRAINT settings_updated_by_fkey FOREIGN KEY (updated_by) REFERENCES olp.users(id);

ALTER TABLE ONLY olp.trace_spans
    ADD CONSTRAINT trace_spans_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES olp.api_keys(id);

ALTER TABLE ONLY olp.trace_spans
    ADD CONSTRAINT trace_spans_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.trace_spans
    ADD CONSTRAINT trace_spans_project_id_fkey FOREIGN KEY (project_id) REFERENCES olp.projects(id);

ALTER TABLE ONLY olp.trace_spans
    ADD CONSTRAINT trace_spans_run_id_fkey FOREIGN KEY (run_id) REFERENCES olp.agent_runs(id);

ALTER TABLE ONLY olp.workflow_model_leases
    ADD CONSTRAINT workflow_model_leases_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id) ON DELETE CASCADE;

ALTER TABLE ONLY olp.workflow_model_nonacceptance_proofs
    ADD CONSTRAINT workflow_model_nonacceptance_proofs_dispatch_id_fkey FOREIGN KEY (dispatch_id) REFERENCES olp.execution_debits(id);

ALTER TABLE ONLY olp.workload_issuers
    ADD CONSTRAINT workload_issuers_created_by_fkey FOREIGN KEY (created_by) REFERENCES olp.users(id);

INSERT INTO olp.async_worker_counters(singleton) VALUES(true);
INSERT INTO olp.capture_configuration(singleton) VALUES(true);
INSERT INTO olp.commitment_cutover(at) VALUES(now());
INSERT INTO olp.budget_calendar_changes SELECT kind,'1970-01-01 00:00:00+00','UTC' FROM unnest(ARRAY['day','week','month']) kind;

SET LOCAL check_function_bodies TO DEFAULT;
