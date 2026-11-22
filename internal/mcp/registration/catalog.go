package registration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/domain/toolidentity"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/routing"
)

type modifiedTool struct {
	Name                  string          `json:"name"`
	ChangedFields         []string        `json:"changed_fields"`
	SavedDescription      string          `json:"saved_description"`
	DiscoveredDescription string          `json:"discovered_description"`
	SavedInputSchema      json.RawMessage `json:"saved_input_schema"`
	DiscoveredInputSchema json.RawMessage `json:"discovered_input_schema"`
}

type toolsetReference struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	RevisionID string   `json:"revision_id"`
	Tools      []string `json:"tools"`
}

type catalogPreview struct {
	ServerID           string             `json:"server_id"`
	ETag               string             `json:"etag"`
	RevisionID         string             `json:"revision_id"`
	SavedProtocol      string             `json:"saved_protocol_version"`
	SavedDigest        string             `json:"saved_digest"`
	DiscoveredProtocol string             `json:"discovered_protocol_version"`
	DiscoveredDigest   string             `json:"discovered_digest"`
	Added              []routing.MCPTool  `json:"added"`
	Removed            []routing.MCPTool  `json:"removed"`
	Modified           []modifiedTool     `json:"modified"`
	Toolsets           []toolsetReference `json:"toolsets"`
	Revision           int64              `json:"revision"`
}

type catalogApproval struct {
	ProtocolVersion string `json:"protocol_version"`
	Digest          string `json:"digest"`
}

func (a catalogApproval) validate() error {
	if a.ProtocolVersion == "" {
		return httpkit.Invalid("protocol_version", "Supply the protocol version from the reviewed catalog preview.")
	}
	if !toolidentity.ValidCatalogDigest(a.Digest) {
		return httpkit.Invalid("digest", "Supply the catalog digest from the reviewed catalog preview.")
	}
	return nil
}

func cloneTool(tool routing.MCPTool) routing.MCPTool {
	return routing.MCPTool{Name: tool.Name, Description: tool.Description, InputSchema: slices.Clone(tool.InputSchema)}
}

func jsonEqual(left, right json.RawMessage) bool {
	decode := func(raw json.RawMessage) (any, error) {
		var value any
		err := json.Unmarshal(raw, &value)
		return value, err
	}
	leftValue, leftErr := decode(left)
	rightValue, rightErr := decode(right)
	if leftErr != nil || rightErr != nil {
		return bytes.Equal(left, right)
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func diffCatalogs(saved, discovered []routing.MCPTool) ([]routing.MCPTool, []routing.MCPTool, []modifiedTool) {
	savedByName := make(map[string]routing.MCPTool, len(saved))
	for _, tool := range saved {
		savedByName[tool.Name] = tool
	}
	discoveredNames := make(map[string]bool, len(discovered))
	added := []routing.MCPTool{}
	modified := []modifiedTool{}
	for _, tool := range discovered {
		discoveredNames[tool.Name] = true
		previous, found := savedByName[tool.Name]
		if !found {
			added = append(added, cloneTool(tool))
			continue
		}
		fields := []string{}
		if previous.Description != tool.Description {
			fields = append(fields, "description")
		}
		if !jsonEqual(previous.InputSchema, tool.InputSchema) {
			fields = append(fields, "input_schema")
		}
		if len(fields) != 0 {
			modified = append(modified, modifiedTool{
				Name: tool.Name, ChangedFields: fields,
				SavedDescription: previous.Description, DiscoveredDescription: tool.Description,
				SavedInputSchema: slices.Clone(previous.InputSchema), DiscoveredInputSchema: slices.Clone(tool.InputSchema),
			})
		}
	}
	removed := []routing.MCPTool{}
	for _, tool := range saved {
		if !discoveredNames[tool.Name] {
			removed = append(removed, cloneTool(tool))
		}
	}
	return added, removed, modified
}

func (s *Server) storedCredential(ctx context.Context, saved *Definition) ([]byte, error) {
	if saved.CredentialID == nil {
		return nil, nil
	}
	material, err := s.access.Keys.Read(ctx, s.access.Pool, s.access.Installation, *saved.CredentialID, secrets.MCPCredential)
	if err != nil {
		return nil, httpkit.Fail(422, "mcp_credential_unavailable", "The registered credential is unavailable.")
	}
	return material, nil
}

func (s *Server) discoverSaved(ctx context.Context, saved *Definition) (*upstream.Catalog, error) {
	if routing.MCPTransport(saved.Transport) == routing.MCPStdio {
		return nil, httpkit.Invalid("transport", "Catalog review is available for remote MCP registrations.")
	}
	if s.egress == nil {
		return nil, httpkit.Fail(503, "egress_policy_unavailable", "Certification requires provider egress policy.")
	}
	material, err := s.storedCredential(ctx, saved)
	if err != nil {
		return nil, err
	}
	defer clear(material)
	catalog, err := upstream.CertifyOptions(ctx, s.egress, saved.Endpoint, material, upstream.TransportOptions{Transport: routing.MCPTransport(saved.Transport), Stdio: saved.Stdio.Clone(), Executables: s.executables})
	if err != nil {
		return nil, httpkit.Fail(422, "mcp_certification_failed", "The bounded MCP handshake or tool-schema certification failed.")
	}
	return catalog, nil
}

func (s *Server) referencingToolsets(ctx context.Context, p authz.Principal, saved *Definition) ([]toolsetReference, error) {
	rows, err := s.access.Pool.Query(ctx, "SELECT p.id::text,p.name,p.latest_revision_id::text,v.definition FROM olp.agent_publications p JOIN olp.agent_publication_revisions v ON v.id=p.latest_revision_id WHERE p.kind='toolset' AND NOT p.retired AND p.project_id=$1 AND ($2 OR p.project_id=ANY($3::uuid[])) ORDER BY p.name,p.id", saved.ProjectID, p.AllProjects, p.ProjectIDs())
	if err != nil {
		return nil, err
	}
	type published struct {
		definition         agentcontract.Draft
		id, name, revision string
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (published, error) {
		var item published
		scanErr := row.Scan(&item.id, &item.name, &item.revision, &item.definition)
		return item, scanErr
	})
	if err != nil {
		return nil, err
	}
	references := []toolsetReference{}
	for _, item := range items {
		if item.definition.Toolset == nil {
			continue
		}
		tools := []string{}
		for _, tool := range item.definition.Toolset.Tools {
			if tool.ServerID == saved.ID && !slices.Contains(tools, tool.Name) {
				tools = append(tools, tool.Name)
			}
		}
		if len(tools) != 0 {
			slices.Sort(tools)
			references = append(references, toolsetReference{ID: item.id, Name: item.name, RevisionID: item.revision, Tools: tools})
		}
	}
	return references, nil
}

func retired() error {
	return httpkit.Fail(409, "mcp_server_retired", "Create a new registration instead of changing retained history.")
}

func (s *Server) previewCatalog(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "server_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	saved, err := Load(r.Context(), s.access.Pool, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&saved.ProjectID, authz.Change); err != nil {
		return httpkit.Reply{}, err
	}
	if saved.RetiredAt != nil {
		return httpkit.Reply{}, retired()
	}
	catalog, err := s.discoverSaved(r.Context(), saved)
	if err != nil {
		return httpkit.Reply{}, err
	}
	added, removed, modified := diffCatalogs(saved.Catalog.Tools, catalog.Tools)
	toolsets, err := s.referencingToolsets(r.Context(), p, saved)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(catalogPreview{
		ServerID: saved.ID, ETag: saved.ETag, RevisionID: saved.RevisionID, Revision: saved.Revision,
		SavedProtocol: saved.Catalog.Protocol, SavedDigest: saved.Catalog.Digest,
		DiscoveredProtocol: catalog.Protocol, DiscoveredDigest: catalog.Digest,
		Added: added, Removed: removed, Modified: modified, Toolsets: toolsets,
	}, saved.ETag), nil
}

func retainedToolPrices(prices map[string]string, catalog *upstream.Catalog) map[string]string {
	retained := map[string]string{}
	for _, tool := range catalog.Tools {
		if price, found := prices[tool.Name]; found {
			retained[tool.Name] = price
		}
	}
	return retained
}

func catalogCandidate(current *Definition, catalog *upstream.Catalog) Definition {
	return Definition{
		ProjectID: current.ProjectID, Name: current.Name, Endpoint: current.Endpoint, Transport: current.Transport,
		Enabled: current.Enabled, Stdio: current.Stdio.Clone(), Catalog: catalog,
		CredentialID: current.CredentialID, DelegationKeyID: current.DelegationKeyID, EntraClientSecretID: current.EntraClientSecretID,
		Delegation: current.Delegation.Clone(), OAuth: current.OAuth.Clone(),
		UnitPrice: clonePrice(current.UnitPrice), ToolPrices: retainedToolPrices(current.ToolPrices, catalog),
		CallsPerMinute: current.CallsPerMinute, MaxConcurrency: current.MaxConcurrency,
		PerKeyCallsPerMinute: current.PerKeyCallsPerMinute, PerKeyMaxConcurrency: current.PerKeyMaxConcurrency,
	}
}

func (s *Server) approveCatalog(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "server_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var in catalogApproval
	if err = httpkit.DecodeUnique(r, &in, 64<<10); err != nil {
		return httpkit.Reply{}, err
	}
	if err = in.validate(); err != nil {
		return httpkit.Reply{}, err
	}
	previous, replayed, err := access.Preflight(s.access, r, in, func(tx pgx.Tx, principal authz.Principal) (*Definition, error) {
		return s.loadForWrite(r.Context(), tx, principal, Input{}, id)
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, nil
	}
	if err = httpkit.Match(r, previous.ETag); err != nil {
		return httpkit.Reply{}, err
	}
	if previous.RetiredAt != nil {
		return httpkit.Reply{}, retired()
	}
	catalog, err := s.discoverSaved(r.Context(), previous)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if catalog.Protocol != in.ProtocolVersion || catalog.Digest != in.Digest {
		return httpkit.Reply{}, httpkit.Fail(409, "mcp_catalog_changed", "The live MCP catalog changed since it was reviewed. Preview it again before approving.")
	}
	if catalog.Protocol == previous.Catalog.Protocol && catalog.Digest == previous.Catalog.Digest {
		return httpkit.Reply{}, httpkit.Fail(409, "mcp_catalog_current", "The live MCP catalog already matches the certified catalog.")
	}
	return access.Mutate(s.access, r, in, func(tx pgx.Tx, principal authz.Principal) (*Definition, error) {
		return s.loadForWrite(r.Context(), tx, principal, Input{}, id)
	}, func(tx pgx.Tx, principal authz.Principal, current *Definition) (httpkit.Reply, access.Event, error) {
		if err := httpkit.Match(r, current.ETag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		saved, saveErr := Save(r.Context(), tx, principal.UserID(), current, catalogCandidate(current, catalog))
		if saveErr != nil {
			return httpkit.Reply{}, access.Event{}, saveErr
		}
		change := registrationChange{id: saved.ID, etag: saved.ETag, action: "mcp_server.certify"}
		reply, publishErr := s.publishChange(r.Context(), tx, principal, change, current, Replaced{}, false)
		return reply, access.Event{Action: change.action, Resource: "mcp_server", ID: change.id}, publishErr
	})
}
