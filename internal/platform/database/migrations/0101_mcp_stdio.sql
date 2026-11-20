CREATE TABLE olp.mcp_executable_permissions (
    executable text NOT NULL CHECK (executable ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'),
    digest text NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    approved_by uuid NOT NULL REFERENCES olp.users,
    approved_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (executable,digest)
);

ALTER TABLE olp.mcp_servers DROP CONSTRAINT mcp_servers_transport_check;
ALTER TABLE olp.mcp_servers ADD CONSTRAINT mcp_servers_transport_check CHECK (transport IN ('streamable_http','sse','stdio'));
ALTER TABLE olp.mcp_server_revisions ADD COLUMN stdio jsonb;
ALTER TABLE olp.mcp_servers DROP CONSTRAINT mcp_servers_endpoint_check;
ALTER TABLE olp.mcp_servers ADD CONSTRAINT mcp_servers_endpoint_check
    CHECK ((transport='stdio' AND endpoint='') OR (transport<>'stdio' AND length(endpoint) BETWEEN 1 AND 2048));
