CREATE TABLE olp.mcp_call_identities (
 api_key_id uuid NOT NULL REFERENCES olp.api_keys,
 project_id uuid NOT NULL REFERENCES olp.projects,
 call_digest text NOT NULL CHECK(call_digest ~ '^[a-f0-9]{64}$'),
 dispatch_id uuid NOT NULL UNIQUE REFERENCES olp.execution_debits,
 attempt_id uuid NOT NULL,
 toolset_id uuid NOT NULL REFERENCES olp.agent_publications,
 toolset_revision_id uuid NOT NULL REFERENCES olp.agent_publication_revisions,
 tool_alias text NOT NULL CHECK(tool_alias ~ '^t_[a-f0-9]{32}_[a-f0-9]{32}$'),
 input_digest text NOT NULL CHECK(input_digest ~ '^[a-f0-9]{64}$'),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY (api_key_id, call_digest),
 CHECK(expires_at>created_at)
);
CREATE INDEX mcp_call_identities_expiry ON olp.mcp_call_identities(expires_at);
