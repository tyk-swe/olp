ALTER TABLE olp.managed_export_sinks DROP CONSTRAINT managed_export_sinks_streams_check,
 ADD CONSTRAINT managed_export_sinks_streams_check CHECK (
  cardinality(streams) BETWEEN 1 AND 4
  AND streams <@ ARRAY['requests','attempts','guardrail_decisions','audit','runs','trace_spans']::text[]
  AND (project_id IS NOT NULL OR NOT (streams && ARRAY['runs','trace_spans']::text[]))
 );

ALTER TABLE olp.managed_export_deliveries
 ADD COLUMN evidence_source_id uuid,
 ADD COLUMN evidence_project_id uuid;

CREATE FUNCTION olp.enqueue_managed_workflow_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
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
   'run_id',NEW.run_id,'attempt_id',NEW.attempt_id,'started_nanos',NEW.started_nanos::text,
   'ended_nanos',NEW.ended_nanos::text,'category',NEW.category,'status',NEW.status,
   'attributes',NEW.attributes,'received_at',NEW.received_at);
 END IF;
 INSERT INTO olp.managed_export_deliveries(sink_id,event_id,stream,payload,evidence_source_id,evidence_project_id)
 SELECT id,event,stream_name,body,NEW.id,NEW.project_id FROM olp.managed_export_sinks
 WHERE enabled AND retired_at IS NULL AND project_id=NEW.project_id AND stream_name=ANY(streams);
 RETURN NEW;
END $$;

CREATE TRIGGER export_managed_run_evidence AFTER INSERT ON olp.agent_runs
 FOR EACH ROW EXECUTE FUNCTION olp.enqueue_managed_workflow_evidence();
CREATE TRIGGER export_managed_trace_evidence AFTER INSERT ON olp.trace_spans
 FOR EACH ROW EXECUTE FUNCTION olp.enqueue_managed_workflow_evidence();
