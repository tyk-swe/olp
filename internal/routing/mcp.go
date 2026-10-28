package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/tyk-swe/olp/internal/oif"
)

type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type MCPTransport string

const (
	MCPStreamableHTTP MCPTransport = "streamable_http"
	MCPLegacySSE      MCPTransport = "sse"
	MCPLegacyProtocol              = "2024-11-05"
)

func (t MCPTransport) Valid() bool { return t == MCPStreamableHTTP || t == MCPLegacySSE }

func (t MCPTransport) OrDefault() MCPTransport {
	if t == "" {
		return MCPStreamableHTTP
	}
	return t
}

const (
	DefaultToolCallsPerMinute = int64(60)
	DefaultToolMaxConcurrency = int64(8)
	MaxToolCallsPerMinute     = int64(60000)
	MaxToolConcurrency        = int64(1024)
)

type MCPOAuth struct {
	ClientID                    string   `json:"client_id"`
	Audience                    string   `json:"audience"`
	Scopes                      []string `json:"scopes"`
	AuthorizationEndpoint       string   `json:"authorization_endpoint"`
	TokenEndpoint               string   `json:"token_endpoint"`
	DeviceAuthorizationEndpoint string   `json:"device_authorization_endpoint,omitempty"`
}

func (o *MCPOAuth) Clone() *MCPOAuth {
	if o == nil {
		return nil
	}
	return &MCPOAuth{ClientID: o.ClientID, Audience: o.Audience, Scopes: slices.Clone(o.Scopes), AuthorizationEndpoint: o.AuthorizationEndpoint, TokenEndpoint: o.TokenEndpoint, DeviceAuthorizationEndpoint: o.DeviceAuthorizationEndpoint}
}

type MCPServer struct {
	OAuth                *MCPOAuth         `json:"oauth,omitempty"`
	ID                   string            `json:"id"`
	ProjectID            string            `json:"project_id"`
	RevisionID           string            `json:"revision_id"`
	Endpoint             string            `json:"endpoint"`
	Transport            MCPTransport      `json:"transport,omitempty"`
	Protocol             string            `json:"protocol_version"`
	Digest               string            `json:"digest"`
	CredentialID         *string           `json:"credential_id,omitempty"`
	UnitPrice            *string           `json:"unit_price"`
	ToolPrices           map[string]string `json:"tool_prices,omitempty"`
	CallsPerMinute       int64             `json:"calls_per_minute,omitempty"`
	MaxConcurrency       int64             `json:"max_concurrency,omitempty"`
	PerKeyCallsPerMinute int64             `json:"per_key_calls_per_minute,omitempty"`
	PerKeyMaxConcurrency int64             `json:"per_key_max_concurrency,omitempty"`
	Tools                []MCPTool         `json:"tools"`
}

type MCPToolsetTool struct {
	Transport            MCPTransport
	OAuth                *MCPOAuth
	Alias                string
	Name                 string
	Description          string
	InputSchema          json.RawMessage
	ServerID             string
	ServerRevisionID     string
	CatalogDigest        string
	Endpoint             string
	CredentialID         *string
	UnitPrice            *string
	CallsPerMinute       int64
	MaxConcurrency       int64
	PerKeyCallsPerMinute int64
	PerKeyMaxConcurrency int64
	schema               *jsonschema.Schema
}

var toolPricePattern = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,12})?$`)

func ValidToolPrice(value string) bool {
	return toolPricePattern.MatchString(value)
}

func validToolLimit(value, ceiling int64) bool {
	return value >= 0 && value <= ceiling
}

type ToolLimits struct {
	CallsPerMinute       int64
	MaxConcurrency       int64
	PerKeyCallsPerMinute int64
	PerKeyMaxConcurrency int64
}

func ToolLimitOr(value *int64, fallback int64) int64 {
	if value == nil {
		return fallback
	}
	return *value
}

func EffectiveToolLimits(callsPerMinute, maxConcurrency, perKeyCallsPerMinute, perKeyMaxConcurrency int64) ToolLimits {
	or := func(value, fallback int64) int64 {
		if value <= 0 {
			return fallback
		}
		return value
	}
	return ToolLimits{
		CallsPerMinute:       or(callsPerMinute, DefaultToolCallsPerMinute),
		MaxConcurrency:       or(maxConcurrency, DefaultToolMaxConcurrency),
		PerKeyCallsPerMinute: or(perKeyCallsPerMinute, DefaultToolCallsPerMinute),
		PerKeyMaxConcurrency: or(perKeyMaxConcurrency, DefaultToolMaxConcurrency),
	}
}

func (l ToolLimits) WithinInstallation() bool {
	return l.CallsPerMinute >= 1 && l.CallsPerMinute <= MaxToolCallsPerMinute &&
		l.PerKeyCallsPerMinute >= 1 && l.PerKeyCallsPerMinute <= MaxToolCallsPerMinute &&
		l.MaxConcurrency >= 1 && l.MaxConcurrency <= MaxToolConcurrency &&
		l.PerKeyMaxConcurrency >= 1 && l.PerKeyMaxConcurrency <= MaxToolConcurrency
}

type MCPDriftedTool struct {
	Alias            string
	Name             string
	ServerID         string
	ServerRevisionID string
	CatalogDigest    string
}

type MCPToolsetResolution struct {
	Tools   []MCPToolsetTool
	Drifted []MCPDriftedTool
	Invalid []MCPDriftedTool
	Missing []MCPDriftedTool
}

var mcpDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type mcpNoNetwork struct{}

func (mcpNoNetwork) Load(string) (any, error) {
	return nil, errors.New("schema network loads are refused")
}

func compileMCPSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 64 << 10, MaxDepth: 32, MaxNodes: 4096}); err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	obj, ok := doc.(map[string]any)
	if !ok || obj["type"] != "object" {
		return nil, errors.New("tool schema must describe an object")
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(mcpNoNetwork{})
	if err = compiler.AddResource("https://olp.invalid/tool-schema", doc); err != nil {
		return nil, err
	}
	return compiler.Compile("https://olp.invalid/tool-schema")
}

func (t MCPToolsetTool) ValidateArguments(raw json.RawMessage) error {
	if t.schema == nil {
		return errors.New("tool schema is not compiled")
	}
	if _, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 1 << 20, MaxDepth: 64, MaxNodes: 1 << 16}); err != nil {
		return err
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return t.schema.Validate(value)
}

func (s *Snapshot) validateMCPServers() error {
	if len(s.MCPServers) > 128 {
		return errors.New("too many MCP servers in snapshot")
	}
	seen := map[string]bool{}
	for _, server := range s.MCPServers {
		if server.OAuth != nil && !server.OAuth.Valid() {
			return fmt.Errorf("MCP server has invalid OAuth configuration")
		}
		if !validUUID(server.ID) || !validUUID(server.ProjectID) || !validUUID(server.RevisionID) || server.Endpoint == "" || len(server.Endpoint) > 2048 || server.Protocol == "" || !mcpDigest.MatchString(server.Digest) || seen[server.ID] || len(server.Tools) > 128 {
			return fmt.Errorf("MCP server %q is malformed", server.ID)
		}
		if server.CredentialID != nil && !validUUID(*server.CredentialID) {
			return fmt.Errorf("MCP server %q credential reference is malformed", server.ID)
		}
		if server.UnitPrice != nil && !ValidToolPrice(*server.UnitPrice) {
			return fmt.Errorf("MCP server %q unit price is malformed", server.ID)
		}
		if !validToolLimit(server.CallsPerMinute, MaxToolCallsPerMinute) || !validToolLimit(server.PerKeyCallsPerMinute, MaxToolCallsPerMinute) ||
			!validToolLimit(server.MaxConcurrency, MaxToolConcurrency) || !validToolLimit(server.PerKeyMaxConcurrency, MaxToolConcurrency) {
			return fmt.Errorf("MCP server %q tool limits exceed installation limits", server.ID)
		}
		seen[server.ID] = true
		names := map[string]bool{}
		for _, tool := range server.Tools {
			if tool.Name == "" || len(tool.Name) > 128 || len(tool.Description) > 4096 || len(tool.InputSchema) == 0 || len(tool.InputSchema) > 64<<10 || names[tool.Name] {
				return fmt.Errorf("MCP server %q tool is malformed", server.ID)
			}
			names[tool.Name] = true
		}
		if len(server.ToolPrices) > 128 {
			return fmt.Errorf("MCP server %q has too many tool prices", server.ID)
		}
		for name, price := range server.ToolPrices {
			if !names[name] || !ValidToolPrice(price) {
				return fmt.Errorf("MCP server %q tool price is malformed", server.ID)
			}
		}
	}
	return nil
}

func (s *Snapshot) compileMCP() {
	byID := make(map[string]MCPServer, len(s.MCPServers))
	for _, server := range s.MCPServers {
		byID[server.ID] = server
	}
	compiled := map[string]*jsonschema.Schema{}
	s.mcpTools = map[string]MCPToolsetResolution{}
	for _, revision := range s.AgentPublications {
		if revision.Kind != "toolset" || revision.Definition.Toolset == nil {
			continue
		}
		resolution := MCPToolsetResolution{Tools: []MCPToolsetTool{}, Drifted: []MCPDriftedTool{}, Invalid: []MCPDriftedTool{}, Missing: []MCPDriftedTool{}}
		for _, pin := range revision.Definition.Toolset.Tools {
			drifted := MCPDriftedTool{
				Alias: pin.Alias, Name: pin.Name, ServerID: pin.ServerID,
				ServerRevisionID: pin.RevisionID, CatalogDigest: pin.CatalogDigest,
			}
			server, ok := byID[pin.ServerID]
			if !ok || server.ProjectID != revision.ProjectID {
				resolution.Missing = append(resolution.Missing, drifted)
				continue
			}
			if server.RevisionID != pin.RevisionID || server.Digest != pin.CatalogDigest {
				resolution.Drifted = append(resolution.Drifted, drifted)
				continue
			}
			index := slices.IndexFunc(server.Tools, func(tool MCPTool) bool { return tool.Name == pin.Name })
			if index < 0 {
				resolution.Drifted = append(resolution.Drifted, drifted)
				continue
			}
			catalog := server.Tools[index]
			key := pin.ServerID + "\x00" + pin.Name
			schema, cached := compiled[key]
			if !cached {
				var err error
				schema, err = compileMCPSchema(catalog.InputSchema)
				compiled[key] = schema
				if err != nil {
					resolution.Invalid = append(resolution.Invalid, drifted)
					continue
				}
			}
			if schema == nil {
				resolution.Invalid = append(resolution.Invalid, drifted)
				continue
			}
			effective := EffectiveToolLimits(server.CallsPerMinute, server.MaxConcurrency, server.PerKeyCallsPerMinute, server.PerKeyMaxConcurrency)
			price := copyPointer(server.UnitPrice)
			if toolPrice, ok := server.ToolPrices[pin.Name]; ok {
				price = &toolPrice
			}
			resolution.Tools = append(resolution.Tools, MCPToolsetTool{
				Alias: pin.Alias, Name: pin.Name, Description: catalog.Description,
				InputSchema: slices.Clone(catalog.InputSchema), ServerID: pin.ServerID,
				ServerRevisionID: pin.RevisionID, CatalogDigest: pin.CatalogDigest, Endpoint: server.Endpoint, Transport: server.Transport.OrDefault(),
				CredentialID: copyPointer(server.CredentialID), UnitPrice: price, OAuth: server.OAuth.Clone(),
				CallsPerMinute:       effective.CallsPerMinute,
				MaxConcurrency:       effective.MaxConcurrency,
				PerKeyCallsPerMinute: effective.PerKeyCallsPerMinute,
				PerKeyMaxConcurrency: effective.PerKeyMaxConcurrency,
				schema:               schema,
			})
		}
		s.mcpTools[revision.ID] = resolution
	}
}

func (s *Snapshot) MCPToolset(revisionID string) (MCPToolsetResolution, bool) {
	if s == nil {
		return MCPToolsetResolution{}, false
	}
	resolution, ok := s.mcpTools[revisionID]
	if !ok {
		return MCPToolsetResolution{}, false
	}
	tools := slices.Clone(resolution.Tools)
	for i := range tools {
		tools[i].UnitPrice = copyPointer(tools[i].UnitPrice)
		tools[i].CredentialID = copyPointer(tools[i].CredentialID)
		tools[i].OAuth = tools[i].OAuth.Clone()
	}
	return MCPToolsetResolution{
		Tools:   tools,
		Drifted: slices.Clone(resolution.Drifted),
		Invalid: slices.Clone(resolution.Invalid),
		Missing: slices.Clone(resolution.Missing),
	}, true
}

func (o *MCPOAuth) Valid() bool {
	if o == nil || o.ClientID == "" || len(o.ClientID) > 256 || o.Audience == "" || len(o.Audience) > 2048 || o.AuthorizationEndpoint == "" || o.TokenEndpoint == "" || len(o.Scopes) == 0 || len(o.Scopes) > 32 {
		return false
	}
	for _, scope := range o.Scopes {
		if scope == "" || len(scope) > 256 {
			return false
		}
		for _, character := range scope {
			if character < 0x21 || character > 0x7e || character == '"' || character == '\\' {
				return false
			}
		}
	}
	return true
}
