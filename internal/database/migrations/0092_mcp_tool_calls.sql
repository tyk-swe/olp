CREATE TABLE olp.mcp_tool_calls (
 id uuid PRIMARY KEY,
 dispatch_id uuid NOT NULL UNIQUE REFERENCES olp.execution_debits,
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 project_id uuid NOT NULL REFERENCES olp.projects,
 toolset_id uuid NOT NULL REFERENCES olp.agent_publications,
 toolset_revision_id uuid NOT NULL REFERENCES olp.agent_publication_revisions,
 server_id uuid NOT NULL REFERENCES olp.mcp_servers,
 server_revision_id uuid NOT NULL REFERENCES olp.mcp_server_revisions,
 tool_alias text NOT NULL CHECK(tool_alias ~ '^t_[a-f0-9]{32}_[a-f0-9]{32}$'),
 input_digest text NOT NULL CHECK(input_digest ~ '^[a-f0-9]{64}$'),
 result_digest text NOT NULL CHECK(result_digest ~ '^[a-f0-9]{64}$'),
 status text NOT NULL CHECK(status IN ('succeeded','upstream_error','outcome_unknown')),
 is_error boolean NOT NULL DEFAULT false,
 started_at timestamptz NOT NULL,
 completed_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK(completed_at>=started_at)
);
CREATE INDEX mcp_tool_calls_project_created ON olp.mcp_tool_calls(project_id,created_at DESC,id DESC);
CREATE INDEX mcp_tool_calls_key_created ON olp.mcp_tool_calls(api_key_id,created_at DESC,id DESC);
CREATE TRIGGER mcp_tool_calls_immutable BEFORE UPDATE OR DELETE ON olp.mcp_tool_calls
 FOR EACH ROW EXECUTE FUNCTION olp.reject_execution_evidence_mutation();
