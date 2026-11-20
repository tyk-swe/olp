ALTER TABLE olp.mcp_grant_enrollments ALTER COLUMN server_id DROP NOT NULL;
ALTER TABLE olp.mcp_grant_enrollments ADD COLUMN agent_id uuid REFERENCES olp.agent_publications;
ALTER TABLE olp.mcp_grant_enrollments ADD CONSTRAINT mcp_grant_enrollments_resource CHECK (num_nonnulls(server_id, agent_id) = 1);

ALTER TABLE olp.mcp_user_grants ALTER COLUMN server_id DROP NOT NULL;
ALTER TABLE olp.mcp_user_grants ADD COLUMN agent_id uuid REFERENCES olp.agent_publications;
ALTER TABLE olp.mcp_user_grants ADD CONSTRAINT mcp_user_grants_resource CHECK (num_nonnulls(server_id, agent_id) = 1);

ALTER TABLE olp.a2a_tasks ADD COLUMN oauth_binding_digest text CHECK (oauth_binding_digest IS NULL OR oauth_binding_digest ~ '^[0-9a-f]{64}$');
ALTER TABLE olp.agent_credentials ADD COLUMN entra_client boolean NOT NULL DEFAULT false;
