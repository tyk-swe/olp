ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check,
 ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN ('oidc_client','oidc_flow','mutation_replay','provider_credential','notification_secret','provider_continuation','media_job_source','provider_grant_refresh','grant_enrollment','mfa_totp','mfa_webauthn','saml_key','saml_flow','sink_credential'));

CREATE TABLE olp.managed_export_sinks (
    id uuid PRIMARY KEY,
    project_id uuid REFERENCES olp.projects(id),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    type text NOT NULL CHECK (type = 'https'),
    destination text NOT NULL CHECK (length(destination) BETWEEN 1 AND 2048),
    streams text[] NOT NULL CHECK (cardinality(streams) BETWEEN 1 AND 4 AND streams <@ ARRAY['requests','attempts','guardrail_decisions','audit']::text[]),
    enabled boolean NOT NULL,
    credential_id uuid REFERENCES olp.secrets(id),
    etag uuid NOT NULL,
    retired_at timestamptz,
    created_by uuid NOT NULL REFERENCES olp.users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    delivered_total bigint NOT NULL DEFAULT 0,
    failed_total bigint NOT NULL DEFAULT 0,
    expired_total bigint NOT NULL DEFAULT 0,
    last_delivered_at timestamptz,
    CHECK (project_id IS NULL OR NOT ('audit' = ANY(streams)))
);

CREATE TABLE olp.managed_export_deliveries (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    sink_id uuid NOT NULL REFERENCES olp.managed_export_sinks(id),
    event_id uuid NOT NULL,
    stream text NOT NULL,
    payload jsonb NOT NULL CHECK (jsonb_typeof(payload)='object' AND octet_length(payload::text)<=65536),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL DEFAULT now()+interval '7 days',
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    attempts integer NOT NULL DEFAULT 0,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','delivered','cancelled','expired')),
    lease uuid,
    last_error_code text,
    UNIQUE (sink_id,event_id)
);
CREATE UNIQUE INDEX managed_export_sinks_active_name ON olp.managed_export_sinks
 (coalesce(project_id,'00000000-0000-0000-0000-000000000000'::uuid),lower(name)) WHERE retired_at IS NULL;
CREATE INDEX managed_export_deliveries_due ON olp.managed_export_deliveries(next_attempt_at,id) WHERE status='pending';
CREATE INDEX managed_export_deliveries_expiry ON olp.managed_export_deliveries(expires_at,id);

-- Fan-out joins the asynchronous accounting transaction, never admission.
-- No timestamp/sequence cursor can skip a late-committing source transaction.
CREATE FUNCTION olp.enqueue_managed_export(stream_name text, owning_project uuid, body jsonb) RETURNS void LANGUAGE plpgsql AS $$
DECLARE event uuid := gen_random_uuid();
BEGIN
    INSERT INTO olp.managed_export_deliveries(sink_id,event_id,stream,payload)
    SELECT id,event,stream_name,body FROM olp.managed_export_sinks
    WHERE enabled AND retired_at IS NULL AND stream_name=ANY(streams)
      AND (project_id IS NULL OR project_id=owning_project);
END;
$$;

CREATE FUNCTION olp.export_request_metadata() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE owning_project uuid; body jsonb;
BEGIN
    SELECT project_id INTO owning_project FROM olp.api_keys WHERE id=NEW.api_key_id;
    body := jsonb_build_object('request_id',NEW.id,'route',NEW.route_slug,'operation',NEW.operation,
      'surface',NEW.surface,'started_at',NEW.started_at,'completed_at',NEW.completed_at,
      'status_code',NEW.status_code,'error_class',NEW.error_class,'latency_ms',NEW.total_latency_ms,
      'first_byte_ms',NEW.first_byte_ms,'attempt_count',NEW.attempt_count,'origin',NEW.origin);
    PERFORM olp.enqueue_managed_export('requests',owning_project,body);
    IF jsonb_array_length(NEW.policy_decisions)>0 THEN
      PERFORM olp.enqueue_managed_export('guardrail_decisions',owning_project,jsonb_build_object('request_id',NEW.id,'decisions',NEW.policy_decisions));
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER export_request_metadata AFTER INSERT ON olp.requests FOR EACH ROW EXECUTE FUNCTION olp.export_request_metadata();

CREATE FUNCTION olp.export_attempt_metadata() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE TRIGGER export_attempt_metadata AFTER INSERT OR UPDATE OF estimated_cost,unpriced,usage_complete,input_tokens,output_tokens,cached_input_tokens
 ON olp.attempt_usage_facts FOR EACH ROW EXECUTE FUNCTION olp.export_attempt_metadata();

CREATE FUNCTION olp.export_audit_metadata() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM olp.enqueue_managed_export('audit',NULL,jsonb_build_object('audit_id',NEW.id,
      'action',NEW.action,'resource_type',NEW.resource_type,'outcome',NEW.outcome,'occurred_at',NEW.occurred_at));
    RETURN NEW;
END;
$$;
CREATE TRIGGER export_audit_metadata AFTER INSERT ON olp.audit FOR EACH ROW EXECUTE FUNCTION olp.export_audit_metadata();

ALTER TABLE olp.worker_task_health DROP CONSTRAINT worker_task_health_task_check,
 ADD CONSTRAINT worker_task_health_task_check CHECK (task IN ('request_metadata_consumer','maintenance','cost_reconciliation',
 'request_metadata_gateway_epoch_detection','media_reconciliation','notification_delivery','grant_refresh','health_probes','export_delivery','managed_export_delivery'));
