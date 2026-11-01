package mcpservers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/routing"
)

var ErrTransport = errors.New("upstream MCP transport failed")
var protocols = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

func SupportedProtocols() []string { return slices.Clone(protocols) }

type TransportOptions struct {
	Stdio       *routing.MCPStdioBinding
	Executables *Executables
	ClientName  string
	Timeout     time.Duration
	Transport   routing.MCPTransport
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

type Transport struct {
	client                             *http.Client
	endpoint, token, session, protocol string
	nextID                             atomic.Int64
	listChanged                        atomic.Bool
	stream                             *streamTransport
}

func (c *Transport) ListChanged() bool { return c.listChanged.Load() }

func (c *Transport) observeNotification(raw []byte) {
	var note struct {
		Version string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		ID      json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &note) != nil {
		return
	}
	if note.Version == "2.0" && note.Method == "notifications/tools/list_changed" && len(note.ID) == 0 {
		c.listChanged.Store(true)
	}
}

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
	client, legacy, wireErr := openHTTPTransport(ctx, policy, u, string(credential), opts)
	if wireErr != nil {
		return nil, wireErr
	}
	c := &Transport{client: client, endpoint: u.String(), token: string(credential), stream: legacy}
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
	if err := c.call(ctx, 1, "initialize", initializeParams{ProtocolVersion: requested, ClientInfo: clientInfo{Name: opts.ClientName, Version: "1"}}, &init); err != nil || !transportProtocolAllowed(opts.Transport, init.Protocol) || len(init.Capabilities.Tools) == 0 || string(init.Capabilities.Tools) == "null" {
		c.Close(ctx)
		return nil, ErrTransport
	}
	c.protocol = init.Protocol
	if err := c.call(ctx, 0, "notifications/initialized", nil, nil); err != nil {
		c.Close(ctx)
		return nil, ErrTransport
	}
	return c, nil
}

func (c *Transport) Protocol() string { return c.protocol }

func (c *Transport) ListTools(ctx context.Context, cursor string) (ToolList, error) {
	out := ToolList{}
	if err := c.call(ctx, int(c.nextID.Add(1)), "tools/list", toolListParams{Cursor: cursor}, &out); err != nil {
		return ToolList{}, err
	}
	return out, nil
}

func (c *Transport) CallTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error) {
	var raw json.RawMessage
	if err := c.call(ctx, int(c.nextID.Add(1)), "tools/call", toolCallParams{Name: name, Arguments: arguments}, &raw); err != nil {
		return ToolResult{}, err
	}
	return decodeToolResult(raw)
}

func (c *Transport) applyHeaders(h http.Header) {
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	if c.session != "" {
		h.Set("Mcp-Session-Id", c.session)
	}
	if c.protocol != "" {
		h.Set("MCP-Protocol-Version", c.protocol)
	}
}

func (c *Transport) call(ctx context.Context, id int, method string, params any, out any) error {
	if c.stream != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return ErrTransport
		}
		raw, err := c.callStream(ctx, id, method, encoded)
		if err != nil || id == 0 {
			return err
		}
		return decodeResult(raw, id, out)
	}
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != 0 {
		message["id"] = id
	}
	if params != nil {
		message["params"] = params
	}
	body, _ := json.Marshal(message)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return ErrTransport
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	c.applyHeaders(req.Header)
	resp, err := c.client.Do(req)
	if err != nil {
		return ErrTransport
	}
	defer resp.Body.Close()
	if id == 0 {
		if resp.StatusCode != 202 && resp.StatusCode != 204 && resp.StatusCode != 200 {
			return ErrTransport
		}
		return nil
	}
	if resp.StatusCode != 200 {
		return ErrTransport
	}
	if method == "initialize" {
		c.session = resp.Header.Get("Mcp-Session-Id")
		if len(c.session) > 256 || strings.ContainsAny(c.session, "\r\n\x00") {
			return ErrTransport
		}
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return ErrTransport
	}
	reader := &io.LimitedReader{R: resp.Body, N: (1 << 20) + 1}
	if media == "application/json" {
		raw, err := io.ReadAll(reader)
		if err != nil || len(raw) > 1<<20 {
			return ErrTransport
		}
		return decodeResult(raw, id, out)
	}
	if media != "text/event-stream" {
		return ErrTransport
	}
	eventLimit := sseEventLimit(method)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), eventLimit)
	var data []byte
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) != 0 {
				if err = decodeResult(data, id, out); err == nil {
					return nil
				}
				c.observeNotification(data)
				data = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
		}
		if len(data) > eventLimit || reader.N == 0 {
			return ErrTransport
		}
	}
	return ErrTransport
}

func sseEventLimit(method string) int {
	if method == "tools/call" {
		return 1 << 20
	}
	return 128 << 10
}

func decodeResult(raw []byte, id int, out any) error {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 1 << 20, MaxDepth: 64, MaxNodes: 1 << 16}); err != nil {
		return ErrTransport
	}
	var reply struct {
		Version string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Version != "2.0" || reply.ID != id || len(reply.Error) != 0 || len(reply.Result) == 0 || bytes.Equal(reply.Result, []byte("null")) {
		return ErrTransport
	}
	if json.Unmarshal(reply.Result, out) != nil {
		return ErrTransport
	}
	return nil
}

func (c *Transport) Close(ctx context.Context) {
	if c.client != nil {
		defer c.client.CloseIdleConnections()
	}
	if c.stream != nil {
		_ = c.stream.connection.Close()
		if c.stream.process != nil {
			_ = c.stream.process.Close()
		}
		return
	}
	if c.session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return
	}
	c.applyHeaders(r.Header)
	if response, err := c.client.Do(r); err == nil {
		response.Body.Close()
	}
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
