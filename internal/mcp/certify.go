package mcp

import (
	"context"
	"github.com/tyk-swe/olp/internal/access"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	initializeCertifyTimeout = 10 * time.Second
	maxConcurrentCertifies   = 4
)

func certifyTransport(ctx context.Context, transport *mcpservers.Transport, pinnedDigest string) certificationState {
	catalog, err := transport.CertifyCatalog(ctx)
	if err != nil || catalog == nil {
		return certificationInvalid
	}
	if catalog.Digest != pinnedDigest {
		return certificationDrifted
	}
	return certificationCertified
}

func (s *Server) certifyToolsetServers(ctx context.Context, authority access.Authority, resolved resolvedToolset) map[string]certificationState {
	representatives := map[string]routing.MCPToolsetTool{}
	for _, tool := range resolved.tools {
		if _, ok := representatives[tool.ServerID]; !ok {
			representatives[tool.ServerID] = tool
		}
	}
	tools := make([]routing.MCPToolsetTool, 0, len(representatives))
	for _, tool := range representatives {
		tools = append(tools, tool)
	}
	states := make([]certificationState, len(tools))
	ctx, cancel := context.WithTimeout(ctx, initializeCertifyTimeout)
	defer cancel()
	slots := make(chan struct{}, maxConcurrentCertifies)
	var pending sync.WaitGroup
	for index, tool := range tools {
		pending.Add(1)
		go func() {
			defer pending.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				states[index] = certificationInvalid
				return
			}
			states[index] = s.certifyServer(ctx, authority, tool)
		}()
	}
	pending.Wait()
	certified := make(map[string]certificationState, len(tools))
	for index, tool := range tools {
		certified[tool.ServerID] = states[index]
	}
	return certified
}

func (s *Server) certifyServer(ctx context.Context, authority access.Authority, tool routing.MCPToolsetTool) certificationState {
	if s.Egress == nil {
		return certificationInvalid
	}
	credential, failure := s.loadCredential(ctx, authority, tool)
	if failure != nil {
		return certificationInvalid
	}
	if credential != nil {
		defer clear(credential)
	}
	transport, err := mcpservers.DialTransport(ctx, s.Egress, tool.Endpoint, credential, mcpservers.TransportOptions{ClientName: "openllmproxy-certifier", Timeout: initializeCertifyTimeout})
	if err != nil {
		return certificationInvalid
	}
	defer transport.Close(context.WithoutCancel(ctx))
	return certifyTransport(ctx, transport, tool.CatalogDigest)
}
