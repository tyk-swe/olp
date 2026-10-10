ALTER TABLE olp.secrets DROP CONSTRAINT secrets_purpose_check,
 ADD CONSTRAINT secrets_purpose_check CHECK(purpose IN ('oidc_client','oidc_flow','mutation_replay','provider_credential','notification_secret','provider_continuation','media_job_source','provider_grant_refresh','grant_enrollment','mfa_totp','mfa_webauthn','saml_key','saml_flow','sink_credential','mcp_credential'));

CREATE TABLE olp.mcp_servers (
 id uuid PRIMARY KEY,
 project_id uuid NOT NULL REFERENCES olp.projects(id),
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 100),
 transport text NOT NULL CHECK(transport='streamable_http'),
 endpoint text NOT NULL CHECK(length(endpoint) BETWEEN 1 AND 2048),
 enabled boolean NOT NULL,
 credential_id uuid REFERENCES olp.secrets(id),
 latest_revision_id uuid NOT NULL,
 etag uuid NOT NULL,
 retired_at timestamptz,
 created_by uuid NOT NULL REFERENCES olp.users(id),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX mcp_servers_project_name ON olp.mcp_servers(project_id,lower(name)) WHERE retired_at IS NULL;
CREATE TABLE olp.mcp_server_revisions (
 id uuid PRIMARY KEY,
 server_id uuid NOT NULL REFERENCES olp.mcp_servers(id),
 revision bigint NOT NULL CHECK(revision>0),
 endpoint text NOT NULL,
 protocol_version text NOT NULL,
 tools jsonb NOT NULL CHECK(jsonb_typeof(tools)='array' AND jsonb_array_length(tools)<=128 AND octet_length(tools::text)<=1048576),
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 certified_at timestamptz NOT NULL DEFAULT now(),
 created_by uuid NOT NULL REFERENCES olp.users(id),
 UNIQUE(server_id,revision),
 UNIQUE(id,server_id)
);
ALTER TABLE olp.mcp_servers ADD CONSTRAINT mcp_latest_revision_owner FOREIGN KEY(latest_revision_id,id)
 REFERENCES olp.mcp_server_revisions(id,server_id) DEFERRABLE INITIALLY DEFERRED;
CREATE FUNCTION olp.mcp_revision_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 RAISE EXCEPTION 'MCP server revisions are immutable' USING ERRCODE='55000';
END;
$$;
CREATE TRIGGER mcp_revision_immutable BEFORE UPDATE OR DELETE ON olp.mcp_server_revisions
 FOR EACH ROW EXECUTE FUNCTION olp.mcp_revision_immutable();
