package mcpservers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

const LegacyProtocol = routing.MCPLegacyProtocol

type legacyTransport struct {
	connection sdkmcp.Connection
	gate       chan struct{}
}

type legacyRoundTripper struct {
	base       http.RoundTripper
	policy     *egress.Policy
	origin     *url.URL
	credential string
}

func (t legacyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.User != nil || request.URL.Fragment != "" || len(request.URL.String()) > 2048 {
		return nil, ErrTransport
	}
	destination := &url.URL{Scheme: request.URL.Scheme, Host: request.URL.Host, Path: request.URL.Path, RawPath: request.URL.RawPath}
	target, err := t.policy.ValidateEndpoint(destination.String())
	if err != nil || target.Scheme != t.origin.Scheme || !strings.EqualFold(target.Host, t.origin.Host) {
		return nil, ErrTransport
	}
	forward := request.Clone(request.Context())
	forward.Header = request.Header.Clone()
	if t.credential != "" {
		forward.Header.Set("Authorization", "Bearer "+t.credential)
	}
	return t.base.RoundTrip(forward)
}

func (t legacyRoundTripper) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

func requestedTransportProtocol(transport routing.MCPTransport) string {
	if transport == "sse" {
		return LegacyProtocol
	}
	return protocols[0]
}

func transportProtocolAllowed(transport routing.MCPTransport, protocol string) bool {
	if transport == "sse" {
		return protocol == LegacyProtocol
	}
	return slices.Contains(protocols, protocol)
}

func (c *Transport) callLegacy(ctx context.Context, id int, method string, params any, out any) error {
	select {
	case c.legacy.gate <- struct{}{}:
		defer func() { <-c.legacy.gate }()
	case <-ctx.Done():
		return ErrTransport
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return ErrTransport
	}
	requestID, err := legacyRequestID(id)
	if err != nil {
		return ErrTransport
	}
	if err := c.legacy.connection.Write(ctx, &jsonrpc.Request{ID: requestID, Method: method, Params: encoded}); err != nil {
		return ErrTransport
	}
	if id == 0 {
		return nil
	}
	for {
		message, readErr := c.legacy.connection.Read(ctx)
		if readErr != nil {
			return ErrTransport
		}
		switch reply := message.(type) {
		case *jsonrpc.Request:
			if reply.Method == "notifications/tools/list_changed" && !reply.ID.IsValid() {
				c.listChanged.Store(true)
				continue
			}
			return ErrTransport
		case *jsonrpc.Response:
			if reply.ID != requestID {
				return ErrTransport
			}
			raw, encodeErr := jsonrpc.EncodeMessage(reply)
			if encodeErr != nil {
				return ErrTransport
			}
			return decodeResult(raw, id, out)
		default:
			return ErrTransport
		}
	}
}

func legacyRequestID(id int) (jsonrpc.ID, error) {
	if id == 0 {
		return jsonrpc.ID{}, nil
	}
	return jsonrpc.MakeID(float64(id))
}
