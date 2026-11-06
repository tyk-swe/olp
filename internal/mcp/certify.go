package mcp

import (
	"context"
	"sync"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	initializeCertifyTimeout = 10 * time.Second
	maxConcurrentCertifies   = 4
)

func certifyTransport(ctx context.Context, peer upstream.Upstream, credential []byte, pinnedDigest string) sessions.Certification {
	catalog, err := upstream.CertifyCatalog(ctx, peer, credential)
	if err != nil || catalog == nil {
		return sessions.Invalid
	}
	if catalog.Digest != pinnedDigest {
		return sessions.Drifted
	}
	return sessions.Certified
}

func (s *Server) certifyToolsetServers(ctx context.Context, authority keyauthority.Authority, resolved resolvedToolset) map[string]sessions.Certification {
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
	states := make([]sessions.Certification, len(tools))
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
				states[index] = sessions.Invalid
				return
			}
			states[index] = s.executor.certifyServer(ctx, authority, tool)
		}()
	}
	pending.Wait()
	certified := make(map[string]sessions.Certification, len(tools))
	for index, tool := range tools {
		certified[tool.ServerID] = states[index]
	}
	return certified
}

func (s *Executor) certifyServer(ctx context.Context, authority keyauthority.Authority, tool routing.MCPToolsetTool) sessions.Certification {
	if s.connect == nil {
		return sessions.Invalid
	}
	credential, failure := s.loadCredential(ctx, authority, tool)
	if failure != nil {
		return sessions.Invalid
	}
	if credential != nil {
		defer clear(credential)
	}
	transport, err := s.connect(ctx, tool.Endpoint, credential, upstream.TransportOptions{ClientName: "openllmproxy-certifier", Timeout: initializeCertifyTimeout, Transport: tool.Transport, Stdio: tool.Stdio})
	if err != nil {
		return sessions.Invalid
	}
	defer transport.Close(context.WithoutCancel(ctx))
	return certifyTransport(ctx, transport, credential, tool.CatalogDigest)
}
