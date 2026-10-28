package runtime

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/routing"
)

func readMCPServers(ctx context.Context, tx pgx.Tx) ([]routing.MCPServer, error) {
	rows, err := tx.Query(ctx, `SELECT s.id::text,s.project_id::text,v.id::text,v.endpoint,v.protocol_version,v.digest,s.credential_id::text,v.tools,v.unit_price::text,v.tool_prices,v.calls_per_minute,v.max_concurrency,v.per_key_calls_per_minute,v.per_key_max_concurrency FROM olp.mcp_servers s JOIN olp.mcp_server_revisions v ON v.id=s.latest_revision_id WHERE s.enabled AND s.retired_at IS NULL ORDER BY s.id LIMIT 129`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []routing.MCPServer
	for rows.Next() {
		var server routing.MCPServer
		if err = rows.Scan(&server.ID, &server.ProjectID, &server.RevisionID, &server.Endpoint, &server.Protocol, &server.Digest, &server.CredentialID, &server.Tools, &server.UnitPrice, &server.ToolPrices, &server.CallsPerMinute, &server.MaxConcurrency, &server.PerKeyCallsPerMinute, &server.PerKeyMaxConcurrency); err != nil {
			return nil, err
		}
		out = append(out, server)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 128 {
		return nil, errors.New("too many MCP servers for one release")
	}
	return out, nil
}
