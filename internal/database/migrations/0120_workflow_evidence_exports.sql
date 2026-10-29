ALTER TABLE olp.export_sinks
 DROP CONSTRAINT export_sinks_streams_check,
 ADD CONSTRAINT export_sinks_streams_check
 CHECK (cardinality(streams) BETWEEN 1 AND 6 AND streams <@ ARRAY['requests','attempts','usage_rollups','guardrail_decisions','audit','metering','focus','runs','trace_spans']::text[]);

CREATE FUNCTION olp.export_workflow_run() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE route_slug text;
BEGIN
 SELECT slug INTO route_slug FROM olp.route_revisions WHERE id = NEW.route_revision_id;
 PERFORM olp.enqueue_export('runs', NEW.id, NEW.project_id, COALESCE(route_slug, ''), '', NEW.created_at,
  jsonb_build_object('run_id', NEW.id, 'project_id', NEW.project_id,
   'route_revision_id', NEW.route_revision_id, 'agent_revision_id', NEW.revision_id,
   'created_at', NEW.created_at, 'evidence_origin', 'gateway_observed'));
 RETURN NEW;
END $$;
CREATE TRIGGER export_workflow_run AFTER INSERT ON olp.agent_runs
 FOR EACH ROW EXECUTE FUNCTION olp.export_workflow_run();

CREATE FUNCTION olp.export_client_trace_span() RETURNS trigger LANGUAGE plpgsql AS $$
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
CREATE TRIGGER export_client_trace_span AFTER INSERT ON olp.trace_spans
 FOR EACH ROW EXECUTE FUNCTION olp.export_client_trace_span();

