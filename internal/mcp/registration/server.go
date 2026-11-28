package registration

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

type Server struct {
	executables *Executables
	access      *access.Server
	egress      *egress.Policy
}

func New(a *access.Server, policy *egress.Policy, executables *Executables) *Server {
	return &Server{executables: executables, access: a, egress: policy}
}

func (s *Server) Toolset(ctx context.Context, id string) (string, routing.MCPToolsetTool, error) {
	server, err := Load(ctx, s.access.Pool, id, false)
	if err != nil {
		return "", routing.MCPToolsetTool{}, err
	}
	return server.ProjectID, routing.MCPToolsetTool{ServerID: id, Endpoint: server.Endpoint, OAuth: server.OAuth}, nil
}

type Definition struct {
	EntraClientSecretID  *string                  `json:"-"`
	Delegation           *routing.MCPDelegation   `json:"delegation"`
	DelegationKeyID      *string                  `json:"-"`
	Stdio                *routing.MCPStdioBinding `json:"stdio,omitempty"`
	OAuth                *routing.MCPOAuth        `json:"oauth"`
	ID                   string                   `json:"id"`
	ProjectID            string                   `json:"project_id"`
	Name                 string                   `json:"name"`
	Transport            string                   `json:"transport"`
	Endpoint             string                   `json:"endpoint"`
	Enabled              bool                     `json:"enabled"`
	HasCredential        bool                     `json:"has_credential"`
	ETag                 string                   `json:"etag"`
	RevisionID           string                   `json:"revision_id"`
	Revision             int64                    `json:"revision"`
	Catalog              *upstream.Catalog        `json:"catalog"`
	CertifiedAt          time.Time                `json:"certified_at"`
	RetiredAt            *time.Time               `json:"retired_at"`
	UnitPrice            *string                  `json:"unit_price"`
	ToolPrices           map[string]string        `json:"tool_prices"`
	CallsPerMinute       int64                    `json:"calls_per_minute"`
	MaxConcurrency       int64                    `json:"max_concurrency"`
	PerKeyCallsPerMinute int64                    `json:"per_key_calls_per_minute"`
	PerKeyMaxConcurrency int64                    `json:"per_key_max_concurrency"`
	CredentialID         *string                  `json:"-"`
}
type Input struct {
	EntraClientSecret    *string                    `json:"entra_client_secret,omitempty"`
	Delegation           *routing.MCPDelegation     `json:"delegation"`
	DelegationPrivateKey *string                    `json:"delegation_private_key,omitempty"`
	Stdio                *routing.MCPStdioBinding   `json:"stdio,omitempty"`
	OAuth                *routing.MCPOAuth          `json:"oauth"`
	present              map[string]json.RawMessage `json:"-"`
	ProjectID            *string                    `json:"project_id,omitempty"`
	Name                 string                     `json:"name"`
	Transport            string                     `json:"transport,omitempty"`
	Endpoint             string                     `json:"endpoint"`
	Enabled              *bool                      `json:"enabled"`
	Credential           json.RawMessage            `json:"credential,omitempty"`
	UnitPrice            *string                    `json:"unit_price"`
	ToolPrices           map[string]string          `json:"tool_prices,omitempty"`
	CallsPerMinute       *int64                     `json:"calls_per_minute"`
	MaxConcurrency       *int64                     `json:"max_concurrency"`
	PerKeyCallsPerMinute *int64                     `json:"per_key_calls_per_minute"`
	PerKeyMaxConcurrency *int64                     `json:"per_key_max_concurrency"`
}

func (in *Input) UnmarshalJSON(data []byte) error {
	type inputJSON Input
	var decoded inputJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	decoded.present = raw
	*in = Input(decoded)
	return nil
}

func (in Input) Present(field string) bool {
	_, present := in.present[field]
	return present
}

func cloneToolPrices(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return maps.Clone(values)
}

func (s *Server) Register(mux *http.ServeMux) {
	s.registerExecutables(mux)
	a := s.access
	a.Route(mux, "GET /api/v1/mcp-servers", s.list)
	a.Route(mux, "POST /api/v1/mcp-servers", s.create, access.Deadline(20*time.Second))
	a.Route(mux, "GET /api/v1/mcp-servers/{server_id}", s.get)
	a.Route(mux, "GET /api/v1/mcp-servers/{server_id}/revisions/{revision_id}", s.revision)
	a.Route(mux, "PUT /api/v1/mcp-servers/{server_id}", s.update, access.Deadline(20*time.Second))
	a.Route(mux, "POST /api/v1/mcp-servers/{server_id}/catalog-preview", s.previewCatalog, access.Deadline(20*time.Second))
	a.Route(mux, "POST /api/v1/mcp-servers/{server_id}/catalog-approval", s.approveCatalog, access.Deadline(20*time.Second))
	a.Route(mux, "DELETE /api/v1/mcp-servers/{server_id}", s.retire)
}
func (s *Server) list(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	summaryColumns := strings.Replace(columns, "v.tools", "'[]'::jsonb", 1)
	rows, err := s.access.Pool.Query(r.Context(), "SELECT "+summaryColumns+joined+" WHERE s.retired_at IS NULL AND s.id<$1 AND ($2 OR s.project_id=ANY($3::uuid[])) ORDER BY s.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (serverSummary, error) {
		definition, scanErr := scan(row)
		if scanErr != nil {
			return serverSummary{}, scanErr
		}
		return summarize(*definition), nil
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.ListReplyBy(items, page, func(item serverSummary) string { return item.ID }), nil
}
func (s *Server) get(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "server_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	d, err := Load(r.Context(), s.access.Pool, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(d, d.ETag), nil
}

type revisionResponse struct {
	Delegation           *routing.MCPDelegation   `json:"delegation"`
	Stdio                *routing.MCPStdioBinding `json:"stdio"`
	OAuth                *routing.MCPOAuth        `json:"oauth"`
	ID                   string                   `json:"id"`
	ServerID             string                   `json:"server_id"`
	Revision             int64                    `json:"revision"`
	Endpoint             string                   `json:"endpoint"`
	Catalog              upstream.Catalog         `json:"catalog"`
	CertifiedAt          time.Time                `json:"certified_at"`
	UnitPrice            *string                  `json:"unit_price"`
	ToolPrices           map[string]string        `json:"tool_prices"`
	CallsPerMinute       int64                    `json:"calls_per_minute"`
	MaxConcurrency       int64                    `json:"max_concurrency"`
	PerKeyCallsPerMinute int64                    `json:"per_key_calls_per_minute"`
	PerKeyMaxConcurrency int64                    `json:"per_key_max_concurrency"`
}

func (s *Server) revision(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "server_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	revision, err := httpkit.IDParam(r, "revision_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	d, err := Load(r.Context(), s.access.Pool, id, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	var version int64
	catalog := upstream.Catalog{}
	var endpoint string
	var certified time.Time
	var unitPrice *string
	var oauth *routing.MCPOAuth
	var delegation *routing.MCPDelegation
	var stdio *routing.MCPStdioBinding
	toolPrices := map[string]string{}
	err = s.access.Pool.QueryRow(r.Context(), "SELECT revision,endpoint,protocol_version,tools,digest,certified_at,unit_price::text,tool_prices,oauth,stdio,delegation FROM olp.mcp_server_revisions WHERE server_id=$1 AND id=$2", id, revision).Scan(&version, &endpoint, &catalog.Protocol, &catalog.Tools, &catalog.Digest, &certified, &unitPrice, &toolPrices, &oauth, &stdio, &delegation)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if toolPrices == nil {
		toolPrices = map[string]string{}
	}
	return httpkit.Detail(revisionResponse{Delegation: delegation, Stdio: stdio, OAuth: oauth, ID: revision, ServerID: id, Revision: version, Endpoint: endpoint, Catalog: catalog, CertifiedAt: certified, UnitPrice: unitPrice, ToolPrices: toolPrices, CallsPerMinute: d.CallsPerMinute, MaxConcurrency: d.MaxConcurrency, PerKeyCallsPerMinute: d.PerKeyCallsPerMinute, PerKeyMaxConcurrency: d.PerKeyMaxConcurrency}, revision), nil
}
func (s *Server) create(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.write(r, p, "", false)
}
func (s *Server) update(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "server_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return s.write(r, p, id, false)
}
func (s *Server) retire(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "server_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return s.write(r, p, id, true)
}

// preflight authenticates and checks replay/preconditions before network IO,
// then releases every database lock. The commit phase repeats the checks so
// upstream validation cannot race a new owner or draft observation.
func (s *Server) preflight(r *http.Request, in Input, id string) (*Definition, *httpkit.Reply, error) {
	d, replayed, err := access.Preflight(s.access, r, in, func(tx pgx.Tx, p authz.Principal) (*Definition, error) {
		if id == "" && (in.ProjectID == nil || !routing.MCPTransport(in.Transport).Valid()) {
			return nil, problem.Invalid("project_id", "Choose an owning project and a supported MCP transport.")
		}
		return s.loadForWrite(r.Context(), tx, p, in, id)
	})
	if err != nil || replayed != nil || d == nil {
		return d, replayed, err
	}
	if err = httpkit.Match(r, d.ETag); err != nil {
		return nil, nil, err
	}
	if d.RetiredAt != nil {
		return nil, nil, problem.Fail(409, "mcp_server_retired", "Create a new registration instead of changing retained history.")
	}
	if in.Present("stdio") {
		return nil, nil, problem.Invalid("stdio", "The executable binding is immutable.")
	}
	if in.ProjectID != nil || in.Transport != "" {
		return nil, nil, problem.Invalid("project_id", "Project and transport are immutable.")
	}
	return d, nil, nil
}
func definitionTransport(in Input, previous *Definition) routing.MCPTransport {
	if previous != nil {
		return routing.MCPTransport(previous.Transport)
	}
	return routing.MCPTransport(in.Transport)
}

func inheritedInputField[T any](in Input, field string, supplied, current T) T {
	if in.Present(field) {
		return supplied
	}
	return current
}

func mergeAbsentPricingAndLimits(in Input, previous *Definition) Input {
	limit := func(field string, value *int64, current, fallback int64) *int64 {
		if !in.Present(field) {
			return &current
		}
		if value == nil {
			return &fallback
		}
		return value
	}
	return Input{
		Delegation:           inheritedInputField(in, "delegation", in.Delegation, previous.Delegation).Clone(),
		OAuth:                inheritedInputField(in, "oauth", in.OAuth, previous.OAuth).Clone(),
		UnitPrice:            ptr.Clone(inheritedInputField(in, "unit_price", in.UnitPrice, previous.UnitPrice)),
		ToolPrices:           cloneToolPrices(inheritedInputField(in, "tool_prices", in.ToolPrices, previous.ToolPrices)),
		CallsPerMinute:       limit("calls_per_minute", in.CallsPerMinute, previous.CallsPerMinute, routing.DefaultToolCallsPerMinute),
		MaxConcurrency:       limit("max_concurrency", in.MaxConcurrency, previous.MaxConcurrency, routing.DefaultToolMaxConcurrency),
		PerKeyCallsPerMinute: limit("per_key_calls_per_minute", in.PerKeyCallsPerMinute, previous.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute),
		PerKeyMaxConcurrency: limit("per_key_max_concurrency", in.PerKeyMaxConcurrency, previous.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency),
		DelegationPrivateKey: in.DelegationPrivateKey,
		EntraClientSecret:    in.EntraClientSecret,
		Stdio:                in.Stdio,
		present:              in.present,
		ProjectID:            in.ProjectID,
		Name:                 in.Name,
		Transport:            in.Transport,
		Endpoint:             in.Endpoint,
		Enabled:              in.Enabled,
		Credential:           in.Credential,
	}
}

func samePrice(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func limitsOnlyUpdate(in Input, previous *Definition) bool {
	if previous == nil || in.Enabled == nil {
		return false
	}
	if in.Name != previous.Name || in.Endpoint != previous.Endpoint || *in.Enabled != previous.Enabled {
		return false
	}
	if !reflect.DeepEqual(in.Delegation, previous.Delegation) || in.DelegationPrivateKey != nil || in.EntraClientSecret != nil || !reflect.DeepEqual(in.OAuth, previous.OAuth) || len(in.Credential) != 0 || !samePrice(in.UnitPrice, previous.UnitPrice) {
		return false
	}
	if len(in.ToolPrices) != len(previous.ToolPrices) {
		return false
	}
	for name, price := range in.ToolPrices {
		if previous.ToolPrices[name] != price {
			return false
		}
	}
	effective := routing.EffectiveToolLimits(
		ptr.Or(in.CallsPerMinute, routing.DefaultToolCallsPerMinute),
		ptr.Or(in.MaxConcurrency, routing.DefaultToolMaxConcurrency),
		ptr.Or(in.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute),
		ptr.Or(in.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency))
	return effective != routing.EffectiveToolLimits(previous.CallsPerMinute, previous.MaxConcurrency, previous.PerKeyCallsPerMinute, previous.PerKeyMaxConcurrency)
}

func (s *Server) write(r *http.Request, _ authz.Principal, id string, retire bool) (httpkit.Reply, error) {
	in, err := s.prepareWrite(r, id, retire)
	if err != nil {
		return httpkit.Reply{}, err
	}
	previous, replayed, err := s.preflight(r, in, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {
		return *replayed, nil
	}
	if !retire && definitionTransport(in, previous) == routing.MCPStdio && (in.Endpoint != "" || in.OAuth != nil || len(in.Credential) != 0 || !definitionStdio(in, previous).Valid()) {
		return httpkit.Reply{}, problem.Invalid("stdio", "Use only the immutable approved executable binding for stdio.")
	}
	limitsOnly := !retire && limitsOnlyUpdate(in, previous)
	material, catalog, err := s.certifyChange(r.Context(), in, previous, retire, limitsOnly)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer clear(material)
	return access.Mutate(s.access, r, in, func(tx pgx.Tx, principal authz.Principal) (*Definition, error) {
		return s.loadForWrite(r.Context(), tx, principal, in, id)
	}, func(tx pgx.Tx, principal authz.Principal, current *Definition) (httpkit.Reply, access.Event, error) {
		if current != nil {
			if err := httpkit.Match(r, current.ETag); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			if current.ETag != previous.ETag {
				return httpkit.Reply{}, access.Event{}, problem.Fail(412, "etag_mismatch", "The MCP registration changed during certification.")
			}
		}
		change, err := s.saveChange(r.Context(), tx, principal.UserID(), in, current, catalog, material, retire, limitsOnly)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply, err := s.publishChange(r.Context(), tx, principal, change, current, replacedBy(in, retire), retire)
		return reply, access.Event{Action: change.action, Resource: "mcp_server", ID: change.id}, err
	})
}

func (s *Server) publishChange(ctx context.Context, tx pgx.Tx, principal authz.Principal, change registrationChange, current *Definition, replaced Replaced, retire bool) (httpkit.Reply, error) {
	if err := DeleteReplacedCredentials(ctx, tx, current, replaced); err != nil {
		return httpkit.Reply{}, err
	}
	reply, err := changeReply(ctx, tx, change, retire, current == nil)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err := access.AdvanceAuthority(ctx, tx); err != nil {
		return httpkit.Reply{}, err
	}
	if s.access.RuntimePublisher == nil {
		return reply, nil
	}
	return reply, s.access.RuntimePublisher(ctx, tx, principal.UserID())
}

func definitionStdio(in Input, previous *Definition) *routing.MCPStdioBinding {
	if previous != nil {
		return previous.Stdio.Clone()
	}
	return in.Stdio.Clone()
}
