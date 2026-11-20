ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check,
 ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN ('oidc_client','oidc_flow','mutation_replay','provider_credential','notification_secret','provider_continuation','media_job_source','provider_grant_refresh','grant_enrollment','mfa_totp','mfa_webauthn','saml_key','saml_flow','sink_credential'));

ALTER TABLE olp.worker_task_health DROP CONSTRAINT worker_task_health_task_check,
 ADD CONSTRAINT worker_task_health_task_check CHECK(task IN ('request_metadata_consumer','maintenance','cost_reconciliation','request_metadata_gateway_epoch_detection','media_reconciliation','notification_delivery','grant_refresh','health_probes','export_delivery'));
ALTER TABLE olp.audit ADD COLUMN project_id uuid;

CREATE TABLE olp.export_sinks (
 id uuid PRIMARY KEY,
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 project_id uuid REFERENCES olp.projects,
 type text NOT NULL CHECK(type IN ('https','otlp_logs','s3','gcs','azure_blob')),
 destination text NOT NULL CHECK(length(destination) BETWEEN 1 AND 2048),
 credential_id uuid REFERENCES olp.secrets,
 streams text[] NOT NULL CHECK(cardinality(streams) BETWEEN 1 AND 5 AND streams <@ ARRAY['requests','attempts','usage_rollups','guardrail_decisions','audit']::text[]),
 format text NOT NULL CHECK(format IN ('json','jsonl','otlp')),
 filter jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(filter)='object' AND octet_length(filter::text)<=4096),
 enabled boolean NOT NULL DEFAULT true,
 etag uuid NOT NULL,
 created_by uuid NOT NULL REFERENCES olp.users,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK((type IN ('s3','gcs','azure_blob') AND format='jsonl') OR (type='https' AND format='json') OR (type='otlp_logs' AND format='otlp'))
);
CREATE UNIQUE INDEX export_sinks_name ON olp.export_sinks(COALESCE(project_id,'00000000-0000-0000-0000-000000000000'::uuid),name);

CREATE TABLE olp.export_records (
 id uuid PRIMARY KEY,
 stream text NOT NULL,
 source_id uuid NOT NULL,
 project_id uuid,
 route text NOT NULL DEFAULT '',
 outcome text NOT NULL,
 occurred_at timestamptz NOT NULL,
 queued_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 payload jsonb NOT NULL CHECK(octet_length(payload::text)<=1048576),
 UNIQUE(stream,source_id)
);
CREATE INDEX export_records_age ON olp.export_records(occurred_at,id);

CREATE TABLE olp.export_pending (
 sink_id uuid NOT NULL REFERENCES olp.export_sinks ON DELETE CASCADE,
 record_id uuid NOT NULL REFERENCES olp.export_records ON DELETE CASCADE,
 attempts integer NOT NULL DEFAULT 0 CHECK(attempts>=0),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 last_attempt_at timestamptz,
 last_error_code text,
 delivered_at timestamptz,
 PRIMARY KEY(sink_id,record_id)
);
CREATE INDEX export_pending_due ON olp.export_pending(sink_id,next_attempt_at,record_id) WHERE delivered_at IS NULL;

CREATE TABLE olp.export_cursors (
 sink_id uuid NOT NULL REFERENCES olp.export_sinks ON DELETE CASCADE,
 stream text NOT NULL,
 cursor text,
 delivered_total bigint NOT NULL DEFAULT 0 CHECK(delivered_total>=0),
 failed_total bigint NOT NULL DEFAULT 0 CHECK(failed_total>=0),
 gaps_total bigint NOT NULL DEFAULT 0 CHECK(gaps_total>=0),
 last_success_at timestamptz,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(sink_id,stream)
);

CREATE TABLE olp.export_gaps (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 sink_id uuid NOT NULL REFERENCES olp.export_sinks ON DELETE CASCADE,
 stream text NOT NULL,
 record_count bigint NOT NULL CHECK(record_count>0),
 first_occurred_at timestamptz NOT NULL,
 last_occurred_at timestamptz NOT NULL,
 reason text NOT NULL CHECK(reason='retention_expired'),
 recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX export_gaps_sink ON olp.export_gaps(sink_id,recorded_at DESC,id DESC);

CREATE FUNCTION olp.enqueue_export(p_stream text,p_source uuid,p_project uuid,p_route text,p_outcome text,p_occurred timestamptz,p_payload jsonb)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE v_record uuid := gen_random_uuid();
BEGIN
 INSERT INTO olp.export_records(id,stream,source_id,project_id,route,outcome,occurred_at,payload)
 SELECT v_record,p_stream,p_source,p_project,p_route,p_outcome,p_occurred,p_payload
 WHERE EXISTS(SELECT 1 FROM olp.export_sinks s WHERE s.enabled AND p_stream=ANY(s.streams)
  AND (s.project_id IS NULL OR s.project_id=p_project)
  AND (NOT(s.filter?'project') OR s.filter->>'project'=p_project::text)
  AND (NOT(s.filter?'route') OR s.filter->>'route'=p_route)
  AND (NOT(s.filter?'outcome') OR s.filter->>'outcome'=p_outcome))
 ON CONFLICT(stream,source_id) DO NOTHING;
 IF NOT FOUND THEN RETURN; END IF;
 INSERT INTO olp.export_pending(sink_id,record_id)
 SELECT s.id,v_record FROM olp.export_sinks s WHERE s.enabled AND p_stream=ANY(s.streams)
  AND (s.project_id IS NULL OR s.project_id=p_project)
  AND (NOT(s.filter?'project') OR s.filter->>'project'=p_project::text)
  AND (NOT(s.filter?'route') OR s.filter->>'route'=p_route)
  AND (NOT(s.filter?'outcome') OR s.filter->>'outcome'=p_outcome);
END $$;

CREATE FUNCTION olp.export_accounted_request() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE TRIGGER export_accounted_request AFTER UPDATE OF status ON olp.request_metadata_event_receipts
 FOR EACH ROW EXECUTE FUNCTION olp.export_accounted_request();

CREATE FUNCTION olp.export_audit_record() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM olp.enqueue_export('audit',NEW.id,NEW.project_id,'',NEW.outcome,NEW.occurred_at,to_jsonb(NEW));
 RETURN NEW;
END $$;
CREATE TRIGGER export_audit_record AFTER INSERT ON olp.audit FOR EACH ROW EXECUTE FUNCTION olp.export_audit_record();

CREATE FUNCTION olp.maintain_exports(p_limit integer) RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE v_deleted bigint;
BEGIN
 WITH candidates AS MATERIALIZED (
  SELECT r.id,r.stream,r.occurred_at,
   r.occurred_at < now()-make_interval(days=>COALESCE((SELECT s.value::integer FROM olp.settings s WHERE s.key=
    CASE r.stream WHEN 'audit' THEN 'retention.audit_days' WHEN 'usage_rollups' THEN 'retention.usage_days' ELSE 'retention.requests_days' END),
    CASE r.stream WHEN 'audit' THEN 365 WHEN 'usage_rollups' THEN 90 ELSE 30 END)) AS expired
  FROM olp.export_records r
  WHERE NOT EXISTS(SELECT 1 FROM olp.export_pending p WHERE p.record_id=r.id AND p.delivered_at IS NULL)
   OR r.occurred_at < now()-make_interval(days=>COALESCE((SELECT s.value::integer FROM olp.settings s WHERE s.key=
    CASE r.stream WHEN 'audit' THEN 'retention.audit_days' WHEN 'usage_rollups' THEN 'retention.usage_days' ELSE 'retention.requests_days' END),
    CASE r.stream WHEN 'audit' THEN 365 WHEN 'usage_rollups' THEN 90 ELSE 30 END))
  ORDER BY r.occurred_at,r.id LIMIT p_limit FOR UPDATE OF r SKIP LOCKED
 ), gaps AS (
  INSERT INTO olp.export_gaps(sink_id,stream,record_count,first_occurred_at,last_occurred_at,reason)
  SELECT p.sink_id,c.stream,count(*),min(c.occurred_at),max(c.occurred_at),'retention_expired'
  FROM candidates c JOIN olp.export_pending p ON p.record_id=c.id
  WHERE c.expired AND p.delivered_at IS NULL GROUP BY p.sink_id,c.stream
  RETURNING sink_id,stream,record_count
 ), checkpointed_gaps AS (
  INSERT INTO olp.export_cursors(sink_id,stream,gaps_total)
  SELECT sink_id,stream,sum(record_count) FROM gaps GROUP BY sink_id,stream
  ON CONFLICT(sink_id,stream) DO UPDATE SET gaps_total=export_cursors.gaps_total+EXCLUDED.gaps_total,updated_at=now()
  RETURNING sink_id
 ), removed AS (
  DELETE FROM olp.export_records r USING candidates c WHERE r.id=c.id AND (SELECT count(*) FROM checkpointed_gaps)>=0 RETURNING r.id
 ) SELECT count(*) INTO v_deleted FROM removed;
 DELETE FROM olp.export_gaps WHERE recorded_at<now()-make_interval(days=>COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.audit_days'),365));
 RETURN v_deleted;
END $$;
