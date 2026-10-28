ALTER TABLE olp.mcp_servers
 ADD COLUMN calls_per_minute integer NOT NULL DEFAULT 60 CHECK(calls_per_minute BETWEEN 1 AND 60000),
 ADD COLUMN max_concurrency integer NOT NULL DEFAULT 8 CHECK(max_concurrency BETWEEN 1 AND 1024),
 ADD COLUMN per_key_calls_per_minute integer NOT NULL DEFAULT 60 CHECK(per_key_calls_per_minute BETWEEN 1 AND 60000),
 ADD COLUMN per_key_max_concurrency integer NOT NULL DEFAULT 8 CHECK(per_key_max_concurrency BETWEEN 1 AND 1024);
UPDATE olp.mcp_servers s SET calls_per_minute=v.calls_per_minute,max_concurrency=v.max_concurrency,per_key_calls_per_minute=v.per_key_calls_per_minute,per_key_max_concurrency=v.per_key_max_concurrency
FROM olp.mcp_server_revisions v WHERE v.id=s.latest_revision_id;
ALTER TABLE olp.mcp_tool_calls DROP CONSTRAINT mcp_tool_calls_status_check,
 ADD CHECK(status IN ('dispatched','succeeded','upstream_error','outcome_unknown','settled','not_accepted','refused_unpriced'));
