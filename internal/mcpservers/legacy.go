package mcpservers

import (
	"context"
	"encoding/json"
	"io"
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

type streamTransport struct {
	process    io.Closer
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

func openHTTPTransport(ctx context.Context, policy *egress.Policy, endpoint *url.URL, credential string, opts TransportOptions) (*http.Client, *streamTransport, error) {
	base := policy.Client(opts.Timeout)
	if opts.Transport != routing.MCPLegacySSE {
		return base, nil, nil
	}
	client := &http.Client{
		Transport:     legacyRoundTripper{base: base.Transport, policy: policy, origin: endpoint, credential: credential},
		Timeout:       opts.Timeout,
		CheckRedirect: base.CheckRedirect,
	}
	connection, err := (&sdkmcp.SSEClientTransport{Endpoint: endpoint.String(), HTTPClient: client, MaxEventSize: 1 << 20}).Connect(ctx)
	if err != nil {
		client.CloseIdleConnections()
		return nil, nil, ErrTransport
	}
	return client, &streamTransport{connection: connection, gate: make(chan struct{}, 1)}, nil
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

func (c *Transport) callStream(ctx context.Context, id int, method string, params json.RawMessage) (json.RawMessage, error) {
	select {
	case c.stream.gate <- struct{}{}:
		defer func() { <-c.stream.gate }()
	case <-ctx.Done():
		return nil, ErrTransport
	}
	requestID, err := legacyRequestID(id)
	if err != nil {
		return nil, ErrTransport
	}
	if err := c.stream.connection.Write(ctx, &jsonrpc.Request{ID: requestID, Method: method, Params: params}); err != nil {
		return nil, ErrTransport
	}
	if id == 0 {
		return nil, nil
	}
	for {
		message, readErr := c.stream.connection.Read(ctx)
		if readErr != nil {
			return nil, ErrTransport
		}
		switch reply := message.(type) {
		case *jsonrpc.Request:
			if reply.Method == "notifications/tools/list_changed" && !reply.ID.IsValid() {
				c.listChanged.Store(true)
				continue
			}
			return nil, ErrTransport
		case *jsonrpc.Response:
			if reply.ID != requestID {
				return nil, ErrTransport
			}
			raw, encodeErr := jsonrpc.EncodeMessage(reply)
			if encodeErr != nil {
				return nil, ErrTransport
			}
			return json.RawMessage(raw), nil
		default:
			return nil, ErrTransport
		}
	}
}

func legacyRequestID(id int) (jsonrpc.ID, error) {
	if id == 0 {
		return jsonrpc.ID{}, nil
	}
	return jsonrpc.MakeID(float64(id))
}
