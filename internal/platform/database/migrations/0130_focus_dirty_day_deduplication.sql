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
