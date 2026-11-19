ALTER TABLE olp.mcp_servers
    DROP CONSTRAINT mcp_servers_transport_check,
    ADD CONSTRAINT mcp_servers_transport_check CHECK (transport IN ('streamable_http', 'sse'));
