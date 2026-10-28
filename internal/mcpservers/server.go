package mcpservers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Server struct {
	Executables *Executables
	Access      *access.Server
	Egress      *egress.Policy
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
	Catalog              *Catalog                 `json:"catalog"`
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
	EntraClientSecret    *string                  `json:"entra_client_secret,omitempty"`
	Delegation           *routing.MCPDelegation   `json:"delegation"`
	DelegationPrivateKey *string                  `json:"delegation_private_key,omitempty"`
	Stdio                *routing.MCPStdioBinding `json:"stdio,omitempty"`
	OAuth                *routing.MCPOAuth        `json:"oauth"`
	present              map[string]bool          `json:"-"`
	ProjectID            *string                  `json:"project_id,omitempty"`
	Name                 string                   `json:"name"`
	Transport            string                   `json:"transport,omitempty"`
	Endpoint             string                   `json:"endpoint"`
	Enabled              *bool                    `json:"enabled"`
	Credential           json.RawMessage          `json:"credential,omitempty"`
	UnitPrice            *string                  `json:"unit_price"`
	ToolPrices           map[string]string        `json:"tool_prices,omitempty"`
	CallsPerMinute       *int64                   `json:"calls_per_minute"`
	MaxConcurrency       *int64                   `json:"max_concurrency"`
	PerKeyCallsPerMinute *int64                   `json:"per_key_calls_per_minute"`
	PerKeyMaxConcurrency *int64                   `json:"per_key_max_concurrency"`
}

const columns = "s.id::text,s.project_id::text,s.name,s.transport,s.endpoint,s.enabled,s.credential_id,s.etag::text,s.retired_at,v.id::text,v.revision,v.protocol_version,v.tools,v.digest,v.certified_at,v.unit_price::text,v.tool_prices,s.calls_per_minute,s.max_concurrency,s.per_key_calls_per_minute,s.per_key_max_concurrency,v.oauth,v.stdio,v.delegation,s.delegation_key_id::text,s.entra_client_secret_id::text"
const joined = " FROM olp.mcp_servers s JOIN olp.mcp_server_revisions v ON v.id=s.latest_revision_id"

func scan(row pgx.Row) (*Definition, error) {
	d := &Definition{Catalog: &Catalog{}}
	err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Transport, &d.Endpoint, &d.Enabled, &d.CredentialID, &d.ETag, &d.RetiredAt, &d.RevisionID, &d.Revision, &d.Catalog.Protocol, &d.Catalog.Tools, &d.Catalog.Digest, &d.CertifiedAt, &d.UnitPrice, &d.ToolPrices, &d.CallsPerMinute, &d.MaxConcurrency, &d.PerKeyCallsPerMinute, &d.PerKeyMaxConcurrency, &d.OAuth, &d.Stdio, &d.Delegation, &d.DelegationKeyID, &d.EntraClientSecretID)
	d.HasCredential = d.CredentialID != nil
	if d.ToolPrices == nil {
		d.ToolPrices = map[string]string{}
	}
	return d, err
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
	*in = Input(decoded)
	in.present = make(map[string]bool, len(raw))
	for key := range raw {
		in.present[key] = true
	}
	return nil
}

func (in Input) Present(field string) bool {
	return in.present[field]
}

func clonePrice(value *string) *string {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func cloneToolPrices(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for name, price := range values {
		out[name] = price
	}
	return out
}

func definitionFromInput(in Input, catalog *Catalog, signingKeyID, entraSecretID *string) *Definition {
	return &Definition{
		EntraClientSecretID: entraSecretID, DelegationKeyID: signingKeyID, Delegation: in.Delegation.Clone(), OAuth: in.OAuth.Clone(), Stdio: in.Stdio.Clone(), Name: in.Name, Transport: in.Transport, Endpoint: in.Endpoint, Enabled: *in.Enabled, Catalog: catalog,
		UnitPrice: clonePrice(in.UnitPrice), ToolPrices: cloneToolPrices(in.ToolPrices),
		CallsPerMinute:       routing.ToolLimitOr(in.CallsPerMinute, routing.DefaultToolCallsPerMinute),
		MaxConcurrency:       routing.ToolLimitOr(in.MaxConcurrency, routing.DefaultToolMaxConcurrency),
		PerKeyCallsPerMinute: routing.ToolLimitOr(in.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute),
		PerKeyMaxConcurrency: routing.ToolLimitOr(in.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency),
	}
}
func Load(ctx context.Context, q access.Queryer, id string, lock bool) (*Definition, error) {
	query := "SELECT " + columns + joined + " WHERE s.id=$1"
	if lock {
		query += " FOR UPDATE OF s"
	}
	return scan(q.QueryRow(ctx, query, id))
}
func FindActive(ctx context.Context, q access.Queryer, project, name string) (*Definition, error) {
	return scan(q.QueryRow(ctx, "SELECT "+columns+joined+" WHERE s.project_id=$1 AND lower(s.name)=lower($2) AND s.retired_at IS NULL", project, name))
}
func (s *Server) Register(mux *http.ServeMux) {
	s.registerExecutables(mux)
	a := s.Access
	a.Route(mux, "GET /api/v1/mcp-servers", s.list)
	a.Route(mux, "POST /api/v1/mcp-servers", s.create, access.Deadline(20*time.Second))
	a.Route(mux, "GET /api/v1/mcp-servers/{server_id}", s.get)
	a.Route(mux, "GET /api/v1/mcp-servers/{server_id}/revisions/{revision_id}", s.revision)
	a.Route(mux, "PUT /api/v1/mcp-servers/{server_id}", s.update, access.Deadline(20*time.Second))
	a.Route(mux, "DELETE /api/v1/mcp-servers/{server_id}", s.retire)
}
func (s *Server) list(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	// Collection summaries must not load up to 200 full schema catalogs.
	summaryColumns := strings.Replace(columns, "v.tools", "'[]'::jsonb", 1)
	rows, err := s.Access.Pool.Query(r.Context(), "SELECT "+summaryColumns+joined+" WHERE s.retired_at IS NULL AND s.id<$1 AND ($2 OR s.project_id=ANY($3::uuid[])) ORDER BY s.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		d, err := scan(rows)
		if err != nil {
			return access.Reply{}, err
		}
		raw, _ := json.Marshal(d)
		var item map[string]any
		_ = json.Unmarshal(raw, &item)
		delete(item, "catalog")
		items = append(items, item)
	}
	return access.ListReply(items, page), rows.Err()
}
func (s *Server) get(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "server_id")
	if err != nil {
		return access.Reply{}, err
	}
	d, err := Load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(d, d.ETag), nil
}

type revisionResponse struct {
	Delegation           *routing.MCPDelegation   `json:"delegation"`
	Stdio                *routing.MCPStdioBinding `json:"stdio"`
	OAuth                *routing.MCPOAuth        `json:"oauth"`
	ID                   string                   `json:"id"`
	ServerID             string                   `json:"server_id"`
	Revision             int64                    `json:"revision"`
	Endpoint             string                   `json:"endpoint"`
	Catalog              Catalog                  `json:"catalog"`
	CertifiedAt          time.Time                `json:"certified_at"`
	UnitPrice            *string                  `json:"unit_price"`
	ToolPrices           map[string]string        `json:"tool_prices"`
	CallsPerMinute       int64                    `json:"calls_per_minute"`
	MaxConcurrency       int64                    `json:"max_concurrency"`
	PerKeyCallsPerMinute int64                    `json:"per_key_calls_per_minute"`
	PerKeyMaxConcurrency int64                    `json:"per_key_max_concurrency"`
}

func (s *Server) revision(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "server_id")
	if err != nil {
		return access.Reply{}, err
	}
	revision, err := access.IDParam(r, "revision_id")
	if err != nil {
		return access.Reply{}, err
	}
	d, err := Load(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&d.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	var version int64
	catalog := Catalog{}
	var endpoint string
	var certified time.Time
	var unitPrice *string
	var oauth *routing.MCPOAuth
	var delegation *routing.MCPDelegation
	var stdio *routing.MCPStdioBinding
	toolPrices := map[string]string{}
	err = s.Access.Pool.QueryRow(r.Context(), "SELECT revision,endpoint,protocol_version,tools,digest,certified_at,unit_price::text,tool_prices,oauth,stdio,delegation FROM olp.mcp_server_revisions WHERE server_id=$1 AND id=$2", id, revision).Scan(&version, &endpoint, &catalog.Protocol, &catalog.Tools, &catalog.Digest, &certified, &unitPrice, &toolPrices, &oauth, &stdio, &delegation)
	if err != nil {
		return access.Reply{}, err
	}
	if toolPrices == nil {
		toolPrices = map[string]string{}
	}
	return access.Detail(revisionResponse{Delegation: delegation, Stdio: stdio, OAuth: oauth, ID: revision, ServerID: id, Revision: version, Endpoint: endpoint, Catalog: catalog, CertifiedAt: certified, UnitPrice: unitPrice, ToolPrices: toolPrices, CallsPerMinute: d.CallsPerMinute, MaxConcurrency: d.MaxConcurrency, PerKeyCallsPerMinute: d.PerKeyCallsPerMinute, PerKeyMaxConcurrency: d.PerKeyMaxConcurrency}, revision), nil
}
func Validate(in *Input, policy *egress.Policy) error {
	if err := validateEntra(in); err != nil {
		return err
	}
	if err := validateDelegation(in); err != nil {
		return err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err := access.ValidText("name", in.Name, 100); err != nil {
		return err
	}
	if in.Enabled == nil {
		return access.Invalid("enabled", "Supply an explicit enabled flag.")
	}
	if policy == nil {
		return access.Fail(503, "egress_policy_unavailable", "Certification requires provider egress policy.")
	}
	if in.Transport != "" && !routing.MCPTransport(in.Transport).Valid() {
		return access.Invalid("transport", "Use streamable_http, sse or stdio.")
	}
	if in.Stdio != nil || in.Transport == string(routing.MCPStdio) {
		if !in.Stdio.Valid() || in.Endpoint != "" || in.OAuth != nil || len(in.Credential) != 0 {
			return access.Invalid("stdio", "Pin an installed executable and digest without HTTP credentials or endpoint.")
		}
		if in.Transport != "" && in.Transport != string(routing.MCPStdio) {
			return access.Invalid("transport", "A stdio binding requires stdio transport.")
		}
	} else if in.Endpoint != "" || in.Transport != "" {
		u, err := policy.ValidateEndpoint(in.Endpoint)
		if err != nil {
			return access.Invalid("endpoint", "The MCP endpoint is not permitted by provider egress policy.")
		}
		in.Endpoint = u.String()
	}
	if err := validateOAuth(in.OAuth, policy); err != nil {
		return err
	}
	if in.UnitPrice != nil && !routing.ValidToolPrice(*in.UnitPrice) {
		return access.Invalid("unit_price", "Use a non-negative decimal price with at most 12 integer and 12 fractional digits, or null for no price.")
	}
	if len(in.ToolPrices) > 128 {
		return access.Invalid("tool_prices", "Price at most 128 tools.")
	}
	for name, price := range in.ToolPrices {
		if name == "" || len(name) > 128 || !routing.ValidToolPrice(price) {
			return access.Invalid("tool_prices", "Use certified tool names with non-negative decimal prices.")
		}
	}
	for _, limit := range [...]struct {
		field   string
		value   *int64
		ceiling int64
	}{
		{"calls_per_minute", in.CallsPerMinute, routing.MaxToolCallsPerMinute},
		{"per_key_calls_per_minute", in.PerKeyCallsPerMinute, routing.MaxToolCallsPerMinute},
		{"max_concurrency", in.MaxConcurrency, routing.MaxToolConcurrency},
		{"per_key_max_concurrency", in.PerKeyMaxConcurrency, routing.MaxToolConcurrency},
	} {
		if limit.value != nil && (*limit.value < 1 || *limit.value > limit.ceiling) {
			return access.Invalid(limit.field, "Stay inside the installation tool call limits.")
		}
	}
	return nil
}
func Credential(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || len(value) < 1 || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return nil, access.Invalid("credential", "Supply bounded bearer material, or null to remove it.")
	}
	return []byte(value), nil
}
func (s *Server) create(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.write(r, p, "", false)
}
func (s *Server) update(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "server_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.write(r, p, id, false)
}
func (s *Server) retire(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "server_id")
	if err != nil {
		return access.Reply{}, err
	}
	return s.write(r, p, id, true)
}

// preflight authenticates and checks replay/preconditions before network IO,
// then releases every database lock. The commit phase repeats the checks so
// upstream validation cannot race a new owner or draft observation.
func (s *Server) preflight(r *http.Request, in Input, id string) (*Definition, *access.Reply, error) {
	tx, err := s.Access.Begin(r)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return nil, nil, err
	}
	var d *Definition
	if id != "" {
		d, err = Load(r.Context(), tx, id, true)
		if err != nil {
			return nil, nil, err
		}
		if err = p.Project(&d.ProjectID, access.Change); err != nil {
			return nil, nil, err
		}
	} else {
		if in.ProjectID == nil || !routing.MCPTransport(in.Transport).Valid() {
			return nil, nil, access.Invalid("project_id", "Choose an owning project and a supported MCP transport.")
		}
		if err = s.Access.RequireProject(r.Context(), tx, p, in.ProjectID); err != nil {
			return nil, nil, err
		}
	}
	_, replayed, err := s.Access.Replay(r, tx, p, in)
	if err != nil {
		return nil, nil, err
	}
	if replayed != nil {
		reply, err := access.Commit(r, tx, *replayed)
		return d, &reply, err
	}
	if d != nil {
		if err = access.Match(r, d.ETag); err != nil {
			return nil, nil, err
		}
		if d.RetiredAt != nil {
			return nil, nil, access.Fail(409, "mcp_server_retired", "Create a new registration instead of changing retained history.")
		}
		if in.Present("stdio") {
			return nil, nil, access.Invalid("stdio", "The executable binding is immutable.")
		}
		if in.ProjectID != nil || in.Transport != "" {
			return nil, nil, access.Invalid("project_id", "Project and transport are immutable.")
		}
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
		UnitPrice:            clonePrice(inheritedInputField(in, "unit_price", in.UnitPrice, previous.UnitPrice)),
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
		routing.ToolLimitOr(in.CallsPerMinute, routing.DefaultToolCallsPerMinute),
		routing.ToolLimitOr(in.MaxConcurrency, routing.DefaultToolMaxConcurrency),
		routing.ToolLimitOr(in.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute),
		routing.ToolLimitOr(in.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency))
	return effective != routing.EffectiveToolLimits(previous.CallsPerMinute, previous.MaxConcurrency, previous.PerKeyCallsPerMinute, previous.PerKeyMaxConcurrency)
}

func (s *Server) write(r *http.Request, _ access.Principal, id string, retire bool) (access.Reply, error) {
	var decoded Input
	if !retire {
		if err := access.DecodeUnique(r, &decoded, 64<<10); err != nil {
			return access.Reply{}, err
		}
	}
	in := func() Input {
		if !retire && id != "" {
			if current, loadErr := Load(r.Context(), s.Access.Pool, id, false); loadErr == nil {
				return mergeAbsentPricingAndLimits(decoded, current)
			}
		}
		return decoded
	}()
	if !retire {
		if err := Validate(&in, s.Egress); err != nil {
			return access.Reply{}, err
		}
	}
	previous, replayed, err := s.preflight(r, in, id)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, nil
	}
	if !retire && definitionTransport(in, previous) == routing.MCPStdio && (in.Endpoint != "" || in.OAuth != nil || len(in.Credential) != 0 || !definitionStdio(in, previous).Valid()) {
		return access.Reply{}, access.Invalid("stdio", "Use only the immutable approved executable binding for stdio.")
	}

	var material []byte
	var catalog *Catalog
	limitsOnly := !retire && limitsOnlyUpdate(in, previous)
	if limitsOnly {
		catalog = previous.Catalog
	}
	if !retire && !limitsOnly {
		material, err = Credential(in.Credential)
		if err != nil {
			return access.Reply{}, err
		}
		if len(in.Credential) == 0 && previous != nil && previous.CredentialID != nil {
			if in.Endpoint != previous.Endpoint {
				return access.Reply{}, access.Invalid("credential", "Supply a replacement credential or null when changing the MCP endpoint.")
			}
			material, err = s.Access.Keys.Read(r.Context(), s.Access.Pool, s.Access.Installation, *previous.CredentialID, secrets.MCPCredential)
			if err != nil {
				return access.Reply{}, access.Fail(422, "mcp_credential_unavailable", "The registered credential is unavailable.")
			}
		}
		defer clear(material)
		catalog, err = CertifyOptions(r.Context(), s.Egress, in.Endpoint, material, TransportOptions{Transport: definitionTransport(in, previous), Stdio: definitionStdio(in, previous), Executables: s.Executables})
		if err != nil {
			return access.Reply{}, access.Fail(422, "mcp_certification_failed", "The bounded MCP handshake or tool-schema certification failed.")
		}
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var current *Definition
	if id != "" {
		current, err = Load(r.Context(), tx, id, true)
		if err != nil {
			return access.Reply{}, err
		}
		if err = p.Project(&current.ProjectID, access.Change); err != nil {
			return access.Reply{}, err
		}
	} else {
		if err = s.Access.RequireProject(r.Context(), tx, p, in.ProjectID); err != nil {
			return access.Reply{}, err
		}
	}
	claim, replay, err := s.Access.Replay(r, tx, p, in)
	if err != nil {
		return access.Reply{}, err
	}
	if replay != nil {
		return access.Commit(r, tx, *replay)
	}
	if current != nil {
		if err = access.Match(r, current.ETag); err != nil {
			return access.Reply{}, err
		}
		if current.ETag != previous.ETag {
			return access.Reply{}, access.Fail(412, "etag_mismatch", "The MCP registration changed during certification.")
		}
	}
	action := "mcp_server.update"
	etag := access.NewID()
	if limitsOnly && current != nil {
		effective := routing.EffectiveToolLimits(
			routing.ToolLimitOr(in.CallsPerMinute, routing.DefaultToolCallsPerMinute),
			routing.ToolLimitOr(in.MaxConcurrency, routing.DefaultToolMaxConcurrency),
			routing.ToolLimitOr(in.PerKeyCallsPerMinute, routing.DefaultToolCallsPerMinute),
			routing.ToolLimitOr(in.PerKeyMaxConcurrency, routing.DefaultToolMaxConcurrency))
		if err = UpdateLimits(r.Context(), tx, id, effective, etag); err != nil {
			return access.Reply{}, err
		}
	} else if retire {
		action = "mcp_server.retire"
		_, err = tx.Exec(r.Context(), "UPDATE olp.mcp_servers SET enabled=false,retired_at=now(),credential_id=NULL,delegation_key_id=NULL,entra_client_secret_id=NULL,etag=$2 WHERE id=$1", id, etag)
	} else {
		signingID, signingErr := s.saveDelegationKey(r.Context(), tx, in, current)
		if signingErr != nil {
			return access.Reply{}, signingErr
		}
		entraSecretID, entraSecretErr := s.saveEntraClientSecret(r.Context(), tx, in, current)
		if entraSecretErr != nil {
			return access.Reply{}, entraSecretErr
		}
		next := definitionFromInput(in, catalog, signingID, entraSecretID)
		next.ID = id
		if current == nil {
			next.ProjectID = *in.ProjectID
			action = "mcp_server.create"
		} else {
			next.ProjectID = current.ProjectID
			next.Transport = current.Transport
			next.Stdio = current.Stdio.Clone()
			next.CredentialID = current.CredentialID
		}
		if len(in.Credential) != 0 {
			next.CredentialID = nil
			if len(material) > 0 {
				key := access.NewID()
				next.CredentialID = &key
				if err = s.Access.Keys.Store(r.Context(), tx, s.Access.Installation, key, secrets.MCPCredential, material, nil); err != nil {
					return access.Reply{}, err
				}
			}
		}
		if err = Save(r.Context(), tx, p.UserID(), current, next); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == "23505" {
				return access.Reply{}, access.Fail(409, "mcp_server_exists", "Choose an unused MCP name in this project.")
			}
			return access.Reply{}, err
		}
		id, etag = next.ID, next.ETag
	}
	if err != nil {
		return access.Reply{}, err
	}
	if current != nil && current.CredentialID != nil && (retire || len(in.Credential) != 0) {
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *current.CredentialID, secrets.MCPCredential); err != nil {
			return access.Reply{}, err
		}
	}
	if current != nil && current.DelegationKeyID != nil && (retire || in.Delegation == nil || in.DelegationPrivateKey != nil) {
		if _, keyErr := tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *current.DelegationKeyID, secrets.MCPDelegationSigning); keyErr != nil {
			return access.Reply{}, keyErr
		}
	}
	if current != nil && current.EntraClientSecretID != nil && (retire || in.OAuth == nil || in.OAuth.EntraOBO == nil || in.EntraClientSecret != nil) {
		if _, keyErr := tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2", *current.EntraClientSecretID, secrets.MCPEntraClient); keyErr != nil {
			return access.Reply{}, keyErr
		}
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), action, "mcp_server", id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Reply{Status: 204}
	if !retire {
		next, err := Load(r.Context(), tx, id, false)
		if err != nil {
			return access.Reply{}, err
		}
		reply = access.Detail(next, etag)
		if current == nil {
			reply.Status = 201
			reply.Location = "/api/v1/mcp-servers/" + id
		}
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return access.Reply{}, err
	}
	if s.Access.RuntimePublisher != nil {
		if err = s.Access.RuntimePublisher(r.Context(), tx, p.UserID()); err != nil {
			return access.Reply{}, err
		}
	}
	if err = s.Access.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}

func definitionStdio(in Input, previous *Definition) *routing.MCPStdioBinding {
	if previous != nil {
		return previous.Stdio.Clone()
	}
	return in.Stdio.Clone()
}
