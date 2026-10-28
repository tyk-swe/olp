DROP TRIGGER mcp_tool_calls_immutable ON olp.mcp_tool_calls;
ALTER TABLE olp.mcp_tool_calls ADD COLUMN call_digest text CHECK(call_digest ~ '^[a-f0-9]{64}$');
UPDATE olp.mcp_tool_calls c SET call_digest=i.call_digest FROM olp.mcp_call_identities i WHERE i.dispatch_id=c.dispatch_id;
ALTER TABLE olp.mcp_tool_calls ALTER COLUMN result_digest DROP NOT NULL, ALTER COLUMN completed_at DROP NOT NULL;
ALTER TABLE olp.mcp_tool_calls DROP CONSTRAINT mcp_tool_calls_status_check,
 ADD CHECK(status IN ('dispatched','succeeded','upstream_error','outcome_unknown','settled','not_accepted'));
CREATE FUNCTION olp.guard_mcp_tool_call() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'tool call evidence is immutable'; END IF;
 IF (NEW.id,NEW.dispatch_id,NEW.api_key_id,NEW.project_id,NEW.toolset_id,NEW.toolset_revision_id,NEW.server_id,NEW.server_revision_id,NEW.tool_alias,NEW.input_digest,NEW.call_digest,NEW.started_at,NEW.created_at)
 IS DISTINCT FROM
 (OLD.id,OLD.dispatch_id,OLD.api_key_id,OLD.project_id,OLD.toolset_id,OLD.toolset_revision_id,OLD.server_id,OLD.server_revision_id,OLD.tool_alias,OLD.input_digest,OLD.call_digest,OLD.started_at,OLD.created_at)
 THEN RAISE EXCEPTION 'tool call identity is immutable'; END IF;
 IF NOT ((OLD.status='dispatched' AND NEW.status IN ('succeeded','upstream_error','outcome_unknown'))
      OR (OLD.status='outcome_unknown' AND NEW.status IN ('settled','not_accepted')))
 THEN RAISE EXCEPTION 'tool call status transition is not permitted'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER mcp_tool_calls_guarded BEFORE UPDATE OR DELETE ON olp.mcp_tool_calls
 FOR EACH ROW EXECUTE FUNCTION olp.guard_mcp_tool_call();
