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

type MCPServer struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	RevisionID   string    `json:"revision_id"`
	Endpoint     string    `json:"endpoint"`
	Protocol     string    `json:"protocol_version"`
	Digest       string    `json:"digest"`
	CredentialID *string   `json:"credential_id,omitempty"`
	Tools        []MCPTool `json:"tools"`
}

type MCPToolsetTool struct {
	Alias            string
	Name             string
	Description      string
	InputSchema      json.RawMessage
	ServerID         string
	ServerRevisionID string
	Endpoint         string
	CredentialID     *string
	schema           *jsonschema.Schema
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
		if !validUUID(server.ID) || !validUUID(server.ProjectID) || !validUUID(server.RevisionID) || server.Endpoint == "" || len(server.Endpoint) > 2048 || server.Protocol == "" || !mcpDigest.MatchString(server.Digest) || seen[server.ID] || len(server.Tools) > 128 {
			return fmt.Errorf("MCP server %q is malformed", server.ID)
		}
		if server.CredentialID != nil && !validUUID(*server.CredentialID) {
			return fmt.Errorf("MCP server %q credential reference is malformed", server.ID)
		}
		seen[server.ID] = true
		names := map[string]bool{}
		for _, tool := range server.Tools {
			if tool.Name == "" || len(tool.Name) > 128 || len(tool.Description) > 4096 || len(tool.InputSchema) == 0 || len(tool.InputSchema) > 64<<10 || names[tool.Name] {
				return fmt.Errorf("MCP server %q tool is malformed", server.ID)
			}
			names[tool.Name] = true
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
	s.mcpTools = map[string][]MCPToolsetTool{}
	for _, revision := range s.AgentPublications {
		if revision.Kind != "toolset" || revision.Definition.Toolset == nil {
			continue
		}
		tools := []MCPToolsetTool{}
		for _, pin := range revision.Definition.Toolset.Tools {
			server, ok := byID[pin.ServerID]
			if !ok || server.ProjectID != revision.ProjectID || server.RevisionID != pin.RevisionID || server.Digest != pin.CatalogDigest {
				continue
			}
			index := slices.IndexFunc(server.Tools, func(tool MCPTool) bool { return tool.Name == pin.Name })
			if index < 0 {
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
					continue
				}
			}
			if schema == nil {
				continue
			}
			tools = append(tools, MCPToolsetTool{
				Alias: pin.Alias, Name: pin.Name, Description: catalog.Description,
				InputSchema: slices.Clone(catalog.InputSchema), ServerID: pin.ServerID,
				ServerRevisionID: pin.RevisionID, Endpoint: server.Endpoint,
				CredentialID: copyPointer(server.CredentialID), schema: schema,
			})
		}
		s.mcpTools[revision.ID] = tools
	}
}

func (s *Snapshot) MCPTools(revisionID string) ([]MCPToolsetTool, bool) {
	if s == nil {
		return nil, false
	}
	tools, ok := s.mcpTools[revisionID]
	return slices.Clone(tools), ok
}
