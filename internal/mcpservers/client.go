// Package mcpservers registers and certifies project-owned upstream MCP servers.
// It is separate from the contract-generated management MCP endpoint.
package mcpservers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
)

var ErrCertification = errors.New("upstream MCP certification failed")
var protocols = []string{"2025-11-25", "2025-06-18", "2025-03-26"}
var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}
type Catalog struct {
	Protocol string `json:"protocol_version"`
	Tools    []Tool `json:"tools"`
	Digest   string `json:"digest"`
}
type rpcConnection struct {
	client                             *http.Client
	endpoint, token, session, protocol string
}
type noSchemaNetwork struct{}

func (noSchemaNetwork) Load(string) (any, error) { return nil, ErrCertification }

// Certify negotiates a bounded Streamable HTTP session and captures a stable,
// validated tool catalog. It never invokes tools or runs schema network loaders.
func Certify(ctx context.Context, policy *egress.Policy, endpoint string, credential []byte) (*Catalog, error) {
	if policy == nil {
		return nil, ErrCertification
	}
	u, err := policy.ValidateEndpoint(endpoint)
	if err != nil {
		return nil, ErrCertification
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := &rpcConnection{client: policy.Client(10 * time.Second), endpoint: u.String(), token: string(credential)}
	var init struct {
		Protocol     string `json:"protocolVersion"`
		Capabilities struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"capabilities"`
	}
	if err = c.call(ctx, 1, "initialize", map[string]any{"protocolVersion": protocols[0], "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "openllmproxy-certifier", "version": "1"}}, &init); err != nil || !slices.Contains(protocols, init.Protocol) || len(init.Capabilities.Tools) == 0 || string(init.Capabilities.Tools) == "null" {
		return nil, ErrCertification
	}
	c.protocol = init.Protocol
	defer c.close(ctx)
	if err = c.call(ctx, 0, "notifications/initialized", nil, nil); err != nil {
		return nil, ErrCertification
	}
	result := &Catalog{Protocol: init.Protocol, Tools: []Tool{}}
	seen := map[string]bool{}
	cursors := map[string]bool{}
	cursor := ""
	for page := 0; page < 16; page++ {
		var listing struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Input       json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			Next string `json:"nextCursor"`
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err = c.call(ctx, page+2, "tools/list", params, &listing); err != nil || listing.Tools == nil {
			return nil, ErrCertification
		}
		for _, t := range listing.Tools {
			if !toolName.MatchString(t.Name) || seen[t.Name] || len(t.Description) > 4096 || len(t.Input) > 64<<10 || len(result.Tools) >= 128 {
				return nil, ErrCertification
			}
			// Static authentication is never retained in upstream descriptions or
			// schema defaults, even when a hostile server reflects its bearer value.
			if c.token != "" && (strings.Contains(t.Name, c.token) || strings.Contains(t.Description, c.token) || bytes.Contains(t.Input, credential)) {
				return nil, ErrCertification
			}
			if err = validateSchema(t.Input); err != nil {
				return nil, ErrCertification
			}
			schema, _ := jsonschema.UnmarshalJSON(bytes.NewReader(t.Input))
			if c.token != "" && reflectsCredential(schema, c.token) {
				return nil, ErrCertification
			}
			t.Input, _ = json.Marshal(schema)
			seen[t.Name] = true
			result.Tools = append(result.Tools, Tool{t.Name, t.Description, t.Input})
		}
		if listing.Next == "" {
			sort.Slice(result.Tools, func(i, j int) bool { return result.Tools[i].Name < result.Tools[j].Name })
			encoded, _ := json.Marshal(result.Tools)
			if len(encoded) > 1<<20 {
				return nil, ErrCertification
			}
			sum := sha256.Sum256(encoded)
			result.Digest = hex.EncodeToString(sum[:])
			return result, nil
		}
		if len(listing.Next) > 1024 || cursors[listing.Next] {
			return nil, ErrCertification
		}
		cursors[listing.Next] = true
		cursor = listing.Next
	}
	return nil, ErrCertification
}
func validateSchema(raw json.RawMessage) error {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 64 << 10, MaxDepth: 32, MaxNodes: 4096}); err != nil {
		return ErrCertification
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return ErrCertification
	}
	obj, ok := doc.(map[string]any)
	if !ok || obj["type"] != "object" {
		return ErrCertification
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noSchemaNetwork{})
	if c.AddResource("https://olp.invalid/tool-schema", doc) != nil {
		return ErrCertification
	}
	_, err = c.Compile("https://olp.invalid/tool-schema")
	if err != nil {
		return ErrCertification
	}
	return nil
}
func (c *rpcConnection) call(ctx context.Context, id int, method string, params any, out any) error {
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
		return ErrCertification
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", c.protocol)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return ErrCertification
	}
	defer resp.Body.Close()
	if id == 0 {
		if resp.StatusCode != 202 && resp.StatusCode != 204 && resp.StatusCode != 200 {
			return ErrCertification
		}
		return nil
	}
	if resp.StatusCode != 200 {
		return ErrCertification
	}
	if method == "initialize" {
		c.session = resp.Header.Get("Mcp-Session-Id")
		if len(c.session) > 256 || strings.ContainsAny(c.session, "\r\n\x00") {
			return ErrCertification
		}
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return ErrCertification
	}
	reader := &io.LimitedReader{R: resp.Body, N: (1 << 20) + 1}
	if media == "application/json" {
		raw, err := io.ReadAll(reader)
		if err != nil || len(raw) > 1<<20 {
			return ErrCertification
		}
		return decodeResult(raw, id, out)
	}
	if media != "text/event-stream" {
		return ErrCertification
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 128<<10)
	var data []byte
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) != 0 {
				if err = decodeResult(data, id, out); err == nil {
					return nil
				}
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
		if len(data) > 128<<10 || reader.N == 0 {
			return ErrCertification
		}
	}
	return ErrCertification
}
func decodeResult(raw []byte, id int, out any) error {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 1 << 20, MaxDepth: 64, MaxNodes: 1 << 16}); err != nil {
		return ErrCertification
	}
	var reply struct {
		Version string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Version != "2.0" || reply.ID != id || len(reply.Error) != 0 || len(reply.Result) == 0 || bytes.Equal(reply.Result, []byte("null")) {
		return ErrCertification
	}
	if json.Unmarshal(reply.Result, out) != nil {
		return ErrCertification
	}
	return nil
}

func (c *rpcConnection) close(ctx context.Context) {
	if c.session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return
	}
	r.Header.Set("Mcp-Session-Id", c.session)
	r.Header.Set("MCP-Protocol-Version", c.protocol)
	if c.token != "" {
		r.Header.Set("Authorization", "Bearer "+c.token)
	}
	if response, err := c.client.Do(r); err == nil {
		response.Body.Close()
	}
}

func reflectsCredential(value any, credential string) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(v, credential)
	case []any:
		for _, item := range v {
			if reflectsCredential(item, credential) {
				return true
			}
		}
	case map[string]any:
		for key, item := range v {
			if strings.Contains(key, credential) || reflectsCredential(item, credential) {
				return true
			}
		}
	}
	return false
}
