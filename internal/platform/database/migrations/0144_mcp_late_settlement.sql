-- Late terminal evidence can resolve a call recovered as unknown
CREATE OR REPLACE FUNCTION olp.guard_mcp_tool_call() RETURNS trigger LANGUAGE plpgsql AS $$
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
