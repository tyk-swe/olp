package mcp

import (
	"context"

	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) certifyToolsetServers(ctx context.Context, resolved resolvedToolset) map[string]certificationState {
	representatives := map[string]routing.MCPToolsetTool{}
	for _, tool := range resolved.tools {
		if _, ok := representatives[tool.ServerID]; !ok {
			representatives[tool.ServerID] = tool
		}
	}
	states := make(map[string]certificationState, len(representatives))
	for serverID, tool := range representatives {
		states[serverID] = s.certifyServer(ctx, tool)
	}
	return states
}

func (s *Server) certifyServer(ctx context.Context, tool routing.MCPToolsetTool) certificationState {
	if s.Egress == nil {
		return certificationInvalid
	}
	credential, failure := s.loadCredential(ctx, tool)
	if failure != nil {
		return certificationInvalid
	}
	if credential != nil {
		defer clear(credential)
	}
	catalog, err := mcpservers.Certify(ctx, s.Egress, tool.Endpoint, credential)
	if err != nil {
		return certificationInvalid
	}
	if catalog.Digest != tool.CatalogDigest {
		return certificationDrifted
	}
	return certificationCertified
}
