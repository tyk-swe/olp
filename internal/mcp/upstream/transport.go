package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/mcp/messages"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/routing"
)

var ErrTransport = errors.New("upstream MCP transport failed")

type ExecutableStarter interface {
	Start(context.Context, *routing.MCPStdioBinding, time.Duration) (*plugins.NativeProcess, error)
}

type TransportOptions struct {
	Stdio       *routing.MCPStdioBinding
	Executables ExecutableStarter
	ClientName  string
	Timeout     time.Duration
	Transport   routing.MCPTransport
}

type Dial func(context.Context, string, []byte, TransportOptions) (Upstream, error)

func NewDialer(policy *egress.Policy, executables ExecutableStarter) Dial {
	return func(ctx context.Context, endpoint string, credential []byte, options TransportOptions) (Upstream, error) {
		return DialTransport(ctx, policy, endpoint, credential, TransportOptions{
			Stdio: options.Stdio, Executables: executables, ClientName: options.ClientName, Timeout: options.Timeout, Transport: options.Transport,
		})
	}
}

type ListedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ToolList struct {
	Tools      []ListedTool `json:"tools"`
	NextCursor string       `json:"nextCursor"`
}

type ToolContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type ToolResult struct {
	Content           []ToolContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

type toolListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    struct{}   `json:"capabilities"`
	ClientInfo      clientInfo `json:"clientInfo"`
}

type Upstream interface {
	Protocol() string
	ListTools(ctx context.Context, cursor string) (ToolList, error)
	CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error)
	ListChanged() bool
	Close(ctx context.Context)
}

var _ Upstream = (*Transport)(nil)

type connection interface {
	exchange(context.Context, string, int, string, json.RawMessage) (json.RawMessage, error)
	changed() bool
	close(context.Context, string)
}

type Transport struct {
	wire     connection
	protocol string
	nextID   atomic.Int64
}

func (c *Transport) ListChanged() bool         { return c.wire.changed() }
func (c *Transport) Close(ctx context.Context) { c.wire.close(ctx, c.protocol) }

func DialTransport(ctx context.Context, policy *egress.Policy, endpoint string, credential []byte, opts TransportOptions) (*Transport, error) {
	if opts.ClientName == "" || opts.Timeout <= 0 {
		return nil, ErrTransport
	}
	if opts.Transport == routing.MCPStdio {
		return dialStdio(ctx, opts)
	}
	if policy == nil {
		return nil, ErrTransport
	}
	u, err := policy.ValidateEndpoint(endpoint)
	if err != nil {
		return nil, ErrTransport
	}
	if !opts.Transport.OrDefault().Valid() {
		return nil, ErrTransport
	}
	wire, wireErr := openHTTPTransport(ctx, policy, u, string(credential), opts)
	if wireErr != nil {
		return nil, wireErr
	}
	c := &Transport{wire: wire}
	return finishTransport(ctx, c, opts)
}

func finishTransport(ctx context.Context, c *Transport, opts TransportOptions) (*Transport, error) {
	c.nextID.Store(1)
	var init struct {
		Protocol     string `json:"protocolVersion"`
		Capabilities struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"capabilities"`
	}
	requested := requestedTransportProtocol(opts.Transport)
	if err := exchange(ctx, c, 1, "initialize", initializeParams{ProtocolVersion: requested, ClientInfo: clientInfo{Name: opts.ClientName, Version: "1"}}, &init); err != nil || !transportProtocolAllowed(opts.Transport, init.Protocol) || len(init.Capabilities.Tools) == 0 || string(init.Capabilities.Tools) == "null" {
		c.Close(ctx)
		return nil, ErrTransport
	}
	c.protocol = init.Protocol
	if err := exchange[json.RawMessage, json.RawMessage](ctx, c, 0, "notifications/initialized", nil, nil); err != nil {
		c.Close(ctx)
		return nil, ErrTransport
	}
	return c, nil
}

func (c *Transport) Protocol() string { return c.protocol }

func (c *Transport) ListTools(ctx context.Context, cursor string) (ToolList, error) {
	out := ToolList{}
	if err := exchange(ctx, c, int(c.nextID.Add(1)), "tools/list", toolListParams{Cursor: cursor}, &out); err != nil {
		return ToolList{}, err
	}
	return out, nil
}

func (c *Transport) CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error) {
	var raw json.RawMessage
	if err := exchange(ctx, c, int(c.nextID.Add(1)), "tools/call", toolCallParams{Name: name, Arguments: arguments}, &raw); err != nil {
		return ToolResult{}, err
	}
	return decodeToolResult(raw)
}

func exchange[P, R any](ctx context.Context, c *Transport, id int, method string, params P, out *R) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return ErrTransport
	}
	parameters := func() json.RawMessage {
		if bytes.Equal(encoded, []byte("null")) {
			return nil
		}
		return encoded
	}()
	reply, err := c.wire.exchange(ctx, c.protocol, id, method, parameters)
	if err != nil || id == 0 {
		return err
	}
	return decodeResult(reply, id, out)
}

func matchingResult(raw []byte, id int) (json.RawMessage, error) {
	reply, code := messages.Parse(raw, oif.Limits{MaxBytes: 1 << 20, MaxDepth: 64, MaxNodes: 1 << 16})
	if code != 0 || reply.Kind != messages.Response || string(reply.ID) != strconv.Itoa(id) || len(reply.Error) != 0 || len(reply.Result) == 0 || bytes.Equal(reply.Result, []byte("null")) {
		return nil, ErrTransport
	}
	return reply.Result, nil
}

func decodeResult[T any](raw []byte, id int, out *T) error {
	result, err := matchingResult(raw, id)
	if err != nil || json.Unmarshal(result, out) != nil {
		return ErrTransport
	}
	return nil
}

func decodeToolResult(raw json.RawMessage) (ToolResult, error) {
	var result struct {
		Content []struct {
			Type     string  `json:"type"`
			Text     *string `json:"text,omitempty"`
			Data     *string `json:"data,omitempty"`
			MimeType *string `json:"mimeType,omitempty"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
		IsError           bool            `json:"isError,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.Content == nil {
		return ToolResult{}, ErrTransport
	}
	if len(result.StructuredContent) > 0 && !bytes.Equal(result.StructuredContent, []byte("null")) && result.StructuredContent[0] != '{' {
		return ToolResult{}, ErrTransport
	}
	for _, part := range result.Content {
		switch part.Type {
		case "text":
			if part.Text == nil || part.Data != nil || part.MimeType != nil {
				return ToolResult{}, ErrTransport
			}
		case "image", "audio":
			if part.Text != nil || part.Data == nil || part.MimeType == nil || *part.MimeType == "" {
				return ToolResult{}, ErrTransport
			}
			if _, err := base64.StdEncoding.DecodeString(*part.Data); err != nil {
				return ToolResult{}, ErrTransport
			}
		default:
			return ToolResult{}, ErrTransport
		}
	}
	content := slices.Collect(func(yield func(ToolContent) bool) {
		for _, part := range result.Content {
			if !yield(ToolContent{Type: part.Type, Text: resultString(part.Text), Data: resultString(part.Data), MimeType: resultString(part.MimeType)}) {
				return
			}
		}
	})
	if len(result.Content) == 0 {
		return ToolResult{Content: []ToolContent{}, StructuredContent: result.StructuredContent, IsError: result.IsError}, nil
	}
	return ToolResult{Content: content, StructuredContent: result.StructuredContent, IsError: result.IsError}, nil
}
func resultString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func (c ToolContent) MarshalJSON() ([]byte, error) {
	if c.Type == "text" {
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{c.Type, c.Text})
	}
	return json.Marshal(struct {
		Type     string `json:"type"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}{c.Type, c.Data, c.MimeType})
}
