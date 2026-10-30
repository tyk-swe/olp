// Package routing owns immutable serving configuration, pure validation and
// selection. Runtime owns publication, installation and live authority.
package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/codemode"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/durablecontract"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/mediacontract"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/realtimecontract"
)

// RouteSlug is the published-route identifier carried in model fields.
var RouteSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// Generation identifies one published runtime configuration.
type Generation struct {
	ID          string    `json:"id"`
	Ordinal     int64     `json:"ordinal"`
	ActivatedAt time.Time `json:"activated_at"`
}

// Capability is one certified (model, operation, surface, mode) tuple.
type Capability struct {
	Model     string `json:"model"`
	Operation string `json:"operation"`
	Surface   string `json:"surface"`
	Mode      string `json:"mode"`
	ToolCalls bool   `json:"tool_calls,omitempty"`
}

// Slot is a credential slot pinned to an exact credential version.
type Slot struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Enabled           bool     `json:"enabled"`
	Priority          int      `json:"priority"`
	Weight            int64    `json:"weight"`
	CredentialID      *string  `json:"credential_id"`
	CredentialVersion *int     `json:"credential_version"`
	AllowedModels     []string `json:"allowed_models,omitempty"`
	AllowedRoutes     []string `json:"allowed_routes,omitempty"`
	AllowedAPIKeys    []string `json:"allowed_api_keys,omitempty"`
	RequestsPerMinute *int64   `json:"requests_per_minute,omitempty"`
	TokensPerMinute   *int64   `json:"tokens_per_minute,omitempty"`
	MaxConcurrency    *int64   `json:"max_concurrency,omitempty"`
	Supply
}

// Allows applies the published slot restrictions shared by live requests and
// simulation. Credential authority and transient health are checked by callers.
func (s *Slot) Allows(model, route, keyID string) bool {
	return s.AllowsModelRoute(model, route) &&
		(len(s.AllowedAPIKeys) == 0 || (keyID != "" && slices.Contains(s.AllowedAPIKeys, keyID)))
}

func (s *Slot) AllowsModelRoute(model, route string) bool {
	return s.Enabled &&
		(len(s.AllowedModels) == 0 || slices.Contains(s.AllowedModels, model)) &&
		(len(s.AllowedRoutes) == 0 || slices.Contains(s.AllowedRoutes, route))
}

// Limits is the connection-level quota published for one provider. A nil
// Limits means the connection publishes no quota of its own and a nil member
// means that dimension is unbounded; slots carry their own limits separately.
// Every member is omitted from the serving JSON when unset so a snapshot
// published before connection quotas existed keeps its digest.
type Limits struct {
	RequestsPerMinute *int64 `json:"requests_per_minute,omitempty"`
	TokensPerMinute   *int64 `json:"tokens_per_minute,omitempty"`
	MaxConcurrency    *int64 `json:"max_concurrency,omitempty"`
	Supply
}

// CommitmentPeriods are the UTC calendar periods a commitment can bill on.
var CommitmentPeriods = map[string]bool{"hour": true, "day": true, "month": true}

// Commitment is a provider's provisioned-capacity agreement: an amount owed
// each UTC calendar period, allocated across priced attempts in proportion to
// their list cost.
type Commitment struct {
	ID          string     `json:"id,omitempty"`
	EffectiveAt *time.Time `json:"effective_at,omitempty"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	Amount      string     `json:"amount"`
	Period      string     `json:"period"`
}

// Provider is the serving view of one active provider revision.
type Provider struct {
	CredentialSource string                    `json:"credential_source,omitempty"`
	Network          *egress.ConnectionOptions `json:"network,omitempty"`
	// Plugin is the plugin profile a plugin provider pins as its profile
	// revision, which publication resolves from the installed plugin. A
	// gateway serves the provider from its release and loads the plugin only
	// to run its signing hook and carry its traffic.
	Plugin                   *connectors.PluginProfile        `json:"plugin,omitempty"`
	ProfileID                string                           `json:"profile_id,omitempty"`
	ProfileRevision          string                           `json:"profile_revision,omitempty"`
	SemanticHeaders          map[string]string                `json:"semantic_headers,omitempty"`
	QuerySettings            map[string]string                `json:"query_settings,omitempty"`
	OperationDefaults        map[string]connectors.DefaultSet `json:"operation_defaults,omitempty"`
	Bindings                 map[string]connectors.Binding    `json:"bindings,omitempty"`
	PluginOptions            map[string]string                `json:"plugin_options,omitempty"`
	ID                       string                           `json:"id"`
	Name                     string                           `json:"name"`
	Kind                     string                           `json:"kind"`
	Enabled                  bool                             `json:"enabled"`
	DefaultSlotID            string                           `json:"default_slot_id,omitempty"`
	ActiveCredential         *string                          `json:"active_credential"`
	Capabilities             []Capability                     `json:"capabilities"`
	RevisionID               string                           `json:"revision_id"`
	Endpoint                 string                           `json:"endpoint,omitempty"`
	AuthMode                 string                           `json:"auth_mode,omitempty"`
	CredentialHeaders        []string                         `json:"credential_headers,omitempty"`
	ParameterDefaults        map[string]json.RawMessage       `json:"parameter_defaults,omitempty"`
	CloudRegion              string                           `json:"cloud_region,omitempty"`
	CloudProject             string                           `json:"cloud_project,omitempty"`
	TuningDatasetURIPrefixes []string                         `json:"tuning_dataset_uri_prefixes,omitempty"`
	DocumentURIPrefixes      []string                         `json:"document_uri_prefixes,omitempty"`
	BatchStorage             *connectors.BatchStorage         `json:"batch_storage,omitempty"`
	Deployment               string                           `json:"deployment,omitempty"`
	APIVersion               string                           `json:"api_version,omitempty"`
	Models                   map[string]json.RawMessage       `json:"models,omitempty"`
	// VendorID is the upstream vendor this connection speaks to. It groups
	// connections that share one upstream account for accounting and limits.
	VendorID  string  `json:"vendor_id,omitempty"`
	ProjectID *string `json:"project_id,omitempty"`
	// Limits is the quota shared by every slot of this connection.
	Limits *Limits `json:"limits,omitempty"`
	// Commitment is the provisioned capacity this connection pays for: its
	// attempts price at zero marginal cost and share the commitment amount
	// across each calendar period by list-cost weight.
	Commitment        *Commitment `json:"commitment,omitempty"`
	PendingCommitment *Commitment `json:"pending_commitment,omitempty"`
	// HealthProbe, when set, opts the connection into active probing.
	HealthProbe *HealthProbe `json:"health_probe,omitempty"`
	Slots       []Slot       `json:"slots,omitempty"`
	// ObservedPrincipal is the upstream principal every slot of a provider
	// authenticated by a grant observes, which its serving identity carries;
	// empty for any other provider.
	ObservedPrincipal string `json:"observed_principal,omitempty"`
}

// Target is one route attempt candidate.
type Target struct {
	ID            string `json:"id"`
	ProviderID    string `json:"provider_id"`
	ProviderModel string `json:"provider_model"`
	Priority      int    `json:"priority"`
	Weight        int64  `json:"weight"`
	Timeout       int64  `json:"timeout"`
	RoutingID     string `json:"routing_id"`
	// Tags name the target for route selectors.
	Tags []string `json:"tags,omitempty"`
	// Shadow marks a target that only receives mirrored traffic.
	Shadow *Shadow `json:"shadow,omitempty"`
}

// Route is the latest published revision of one slug.
type Route struct {
	guardrailMigrated     bool
	compiledContentPolicy *contentpolicy.Compiled
	ID                    string    `json:"id"`
	Slug                  string    `json:"slug"`
	Operations            []string  `json:"operations"`
	OverallTimeout        int64     `json:"overall_timeout"`
	MaxAttempts           int       `json:"max_attempts"`
	Targets               []Target  `json:"targets"`
	RoutingID             string    `json:"routing_id"`
	RevisionID            string    `json:"revision_id,omitempty"`
	Revision              int       `json:"revision,omitempty"`
	PublishedAt           time.Time `json:"published_at,omitempty"`
	Policy                *Policy   `json:"policy,omitempty"`
	ProjectID             *string   `json:"project_id,omitempty"`

	ContentPolicy *contentpolicy.Policy `json:"content_policy,omitempty"`
	Fidelity      RouteFidelity         `json:"fidelity"`
	Behavior
}

// Snapshot is the complete immutable serving configuration.
type Snapshot struct {
	AgentPublications    []agentcontract.Revision `json:"agent_publications,omitempty"`
	MCPServers           []MCPServer              `json:"mcp_servers,omitempty"`
	mcpTools             map[string]MCPToolsetResolution
	Commitments          map[string][]Commitment `json:"commitments,omitempty"`
	Guardrails           *guardrails.Registry    `json:"guardrails,omitempty"`
	guardrailEngines     map[string]*guardrails.Engine
	toolGuardrailEngines map[string]*guardrails.Engine
	hasBodyLimits        bool
	CodeRoutes           map[string]codemode.Route `json:"code_routes,omitempty"`
	CodePaidConnections  map[string]Provider       `json:"code_paid_connections,omitempty"`
	CodeConnections      map[string]Configuration  `json:"code_connections,omitempty"`
	codeRevisions        map[string]codeRevision
	interactions         map[string]map[string]*interaction.Template
	operations           map[string]map[string]*operationplan.Template
	media                map[string]map[string]*mediacontract.Template
	durable              map[string]map[string]*durablecontract.Template
	realtime             map[string]map[string]*realtimecontract.Template
	webrtc               map[string]map[string]*realtimecontract.Template
	duplex               map[string]map[string]*duplexcontract.Template
	Generation           Generation          `json:"generation"`
	Providers            map[string]Provider `json:"providers"`
	Routes               map[string]Route    `json:"routes"`
	InstallationPolicy   *Policy             `json:"installation_policy,omitempty"`
	KeyPolicies          map[string]*Policy  `json:"key_policies,omitempty"`
}

// Digest returns the SHA-256 of the canonical JSON encoding of the serving
// content. The generation identity is excluded so republishing unchanged
// configuration yields the same digest.
func (s *Snapshot) Digest() (string, error) {
	encoded, err := json.Marshal(struct {
		AgentPublications   []agentcontract.Revision  `json:"agent_publications,omitempty"`
		MCPServers          []MCPServer               `json:"mcp_servers,omitempty"`
		Commitments         map[string][]Commitment   `json:"commitments,omitempty"`
		Guardrails          *guardrails.Registry      `json:"guardrails,omitempty"`
		CodeRoutes          map[string]codemode.Route `json:"code_routes,omitempty"`
		CodePaidConnections map[string]Provider       `json:"code_paid_connections,omitempty"`
		CodeConnections     map[string]Configuration  `json:"code_connections,omitempty"`
		Providers           map[string]Provider       `json:"providers"`
		Routes              map[string]Route          `json:"routes"`
		InstallationPolicy  *Policy                   `json:"installation_policy,omitempty"`
		KeyPolicies         map[string]*Policy        `json:"key_policies,omitempty"`
	}{s.AgentPublications, s.MCPServers, s.Commitments, s.Guardrails, s.CodeRoutes, s.CodePaidConnections, s.CodeConnections, s.Providers, s.Routes, s.InstallationPolicy, s.KeyPolicies})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Supports reports whether the provider certified the tuple.
func (p *Provider) Supports(model, operation, surface, mode string) bool {
	for _, c := range p.Capabilities {
		if c.Model == model && c.Operation == operation && c.Surface == surface && c.Mode == mode {
			return true
		}
	}
	return false
}

func (p *Provider) SupportsToolCalls(model, surface, mode string) bool {
	return slices.ContainsFunc(p.Capabilities, func(c Capability) bool {
		return c.Model == model && c.Operation == "generation" && c.Surface == surface && c.Mode == mode && c.ToolCalls
	})
}

// validate rejects snapshots that could not serve safely: dangling references,
// non-positive budgets, malformed identifiers.
func (s *Snapshot) validate() error {
	if err := agentcontract.ValidateRevisions(s.AgentPublications); err != nil {
		return err
	}
	if err := s.validateMCPServers(); err != nil {
		return err
	}
	if err := s.Guardrails.Validate(); err != nil {
		return err
	}
	if err := s.validateCodeMode(); err != nil {
		return err
	}
	if _, err := uuid.Parse(s.Generation.ID); err != nil || s.Generation.Ordinal < 0 {
		return errors.New("generation identity is malformed")
	}
	for id, p := range s.Providers {
		if id != p.ID || !validUUID(p.ID) || p.RevisionID == "" || p.Name == "" || p.Kind == "" {
			return fmt.Errorf("provider %q is malformed", id)
		}
		if err := connectors.ValidateCredentialSource(p.CredentialSource, p.Kind, p.Connector().AuthMode); err != nil {
			return err
		}
		if err := p.Connector().ValidateProfile(); err != nil {
			return fmt.Errorf("provider %q profile: %w", id, err)
		}
		if p.Network != nil && p.Network.CredentialID != "" && !validUUID(p.Network.CredentialID) {
			return fmt.Errorf("provider %q network credential reference is malformed", id)
		}
		if p.ActiveCredential != nil && !validUUID(*p.ActiveCredential) {
			return fmt.Errorf("provider %q credential reference is malformed", id)
		}
		seen := map[string]bool{}
		for _, slot := range p.Slots {
			if !validUUID(slot.ID) || seen[slot.ID] || slot.Weight < 1 {
				return fmt.Errorf("provider %q slot is malformed", id)
			}
			seen[slot.ID] = true
			if slot.CredentialID != nil && !validUUID(*slot.CredentialID) {
				return fmt.Errorf("provider %q slot credential reference is malformed", id)
			}
		}
	}
	for slug, r := range s.Routes {
		if slug != r.Slug || !RouteSlug.MatchString(slug) || !validUUID(r.ID) || !validUUID(r.RoutingID) {
			return fmt.Errorf("route %q is malformed", slug)
		}
		if r.OverallTimeout < 1 || r.MaxAttempts < 1 || len(r.Operations) == 0 || len(r.Targets) == 0 {
			return fmt.Errorf("route %q has no serving budget or targets", slug)
		}
		if err := ValidateRouteFidelity(r.Fidelity, r.ContentPolicy); err != nil {
			return fmt.Errorf("route %q fidelity: %w", slug, err)
		}
		if err := r.ValidatePassthrough(); err != nil {
			return fmt.Errorf("route %q: %w", slug, err)
		}
		seen := map[string]bool{}
		for _, t := range r.Targets {
			if !validUUID(t.ID) || !validUUID(t.RoutingID) || seen[t.ID] || t.Weight < 1 || t.Timeout < 1 || t.ProviderModel == "" {
				return fmt.Errorf("route %q target is malformed", slug)
			}
			seen[t.ID] = true
			if _, ok := s.Providers[t.ProviderID]; !ok {
				return fmt.Errorf("route %q references unknown provider %s", slug, t.ProviderID)
			}
		}
		if err := s.validateCallerCostPolicy(r); err != nil {
			return err
		}

	}
	return CheckRouteGraph(s.Routes)
}

// Validate checks published structure and contracts without replacing cached plans.
func (s *Snapshot) Validate() error {
	if err := s.validate(); err != nil {
		return err
	}
	for _, route := range s.Routes {
		if err := s.ValidateRouteContracts(route); err != nil {
			return err
		}
	}
	return nil
}

// Compile installs all derived contracts atomically. A failed compilation leaves
// the previous compiled snapshot untouched.
func (s *Snapshot) Compile() error {
	if err := s.validate(); err != nil {
		return err
	}
	next := *s
	next.Routes = maps.Clone(s.Routes)
	next.Guardrails = s.Guardrails.Clone()
	if err := next.compileGuardrails(); err != nil {
		return err
	}
	if err := next.compileCodeMode(); err != nil {
		return err
	}
	next.compileMCP()
	next.hasBodyLimits = false
	for _, route := range next.Routes {
		if route.MaxBodyBytes != nil {
			next.hasBodyLimits = true
			break
		}
	}
	next.interactions = make(map[string]map[string]*interaction.Template)
	next.operations = make(map[string]map[string]*operationplan.Template)
	next.media = make(map[string]map[string]*mediacontract.Template)
	next.durable = make(map[string]map[string]*durablecontract.Template)
	next.realtime = make(map[string]map[string]*realtimecontract.Template)
	next.webrtc = make(map[string]map[string]*realtimecontract.Template)
	next.duplex = make(map[string]map[string]*duplexcontract.Template)
	for slug, r := range next.Routes {
		r.guardrailMigrated = next.Guardrails != nil && next.Guardrails.MigratedRoutes[r.ID]
		templates, policy, err := next.compileRouteExecution(r)
		if err != nil {
			return fmt.Errorf("route %q interaction: %w", slug, err)
		}
		next.interactions[slug] = templates
		unary, err := next.compileOperations(r)
		if err != nil {
			return err
		}
		next.operations[slug] = unary
		media, err := next.compileMedia(r)
		if err != nil {
			return fmt.Errorf("route %q media: %w", slug, err)
		}
		next.media[slug] = media
		durable, err := next.compileDurable(r)
		if err != nil {
			return fmt.Errorf("route %q durable: %w", slug, err)
		}
		next.durable[slug] = durable
		realtimeTemplates, err := next.compileRealtime(r)
		if err != nil {
			return fmt.Errorf("route %q realtime: %w", slug, err)
		}
		next.realtime[slug] = realtimeTemplates
		calls, err := next.compileWebRTC(r)
		if err != nil {
			return fmt.Errorf("route %q webrtc: %w", slug, err)
		}
		next.webrtc[slug] = calls
		duplex, err := next.compileDuplex(r)
		if err != nil {
			return fmt.Errorf("route %q duplex: %w", slug, err)
		}
		next.duplex[slug] = duplex
		r.compiledContentPolicy = policy
		next.Routes[slug] = r
	}
	*s = next
	return nil
}

func validUUID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil && len(value) == 36
}

func (p *Provider) Connector() connectors.Config {
	mode := p.AuthMode
	if mode == "" {
		mode = "api_key"
	}
	return connectors.Config{Network: p.Network, Plugin: p.Plugin, PluginOptions: p.PluginOptions, ProfileID: p.ProfileID, ProfileRevision: p.ProfileRevision, SemanticHeaders: p.SemanticHeaders, QuerySettings: p.QuerySettings, OperationDefaults: p.OperationDefaults, Bindings: p.Bindings, ObservedPrincipal: p.ObservedPrincipal, CredentialSource: p.CredentialSource, Kind: p.Kind, AuthMode: mode, Endpoint: p.Endpoint, CloudRegion: p.CloudRegion, CloudProject: p.CloudProject, BatchStorage: p.BatchStorage, TuningDatasetURIPrefixes: p.TuningDatasetURIPrefixes, DocumentURIPrefixes: p.DocumentURIPrefixes, Deployment: p.Deployment, APIVersion: p.APIVersion, VendorID: p.VendorID, CredentialHeaders: p.CredentialHeaders, Models: p.Models}
}

// HasBodyLimits reports whether ingress needs byte measurements for ordinary routes.
func (s *Snapshot) HasBodyLimits() bool { return s != nil && s.hasBodyLimits }

// CommitmentAt resolves published effective and pending obligations without IO.
func (p Provider) CommitmentAt(at time.Time) *Commitment {
	for _, c := range []*Commitment{p.PendingCommitment, p.Commitment} {
		if c != nil && (c.EffectiveAt == nil || !at.Before(*c.EffectiveAt)) && (c.EndsAt == nil || at.Before(*c.EndsAt)) {
			return c
		}
	}
	return nil
}

// CommitmentAt also resolves obligations of removed connections, which can
// remain payable after their serving configuration has disappeared.
func (s *Snapshot) CommitmentAt(providerID string, at time.Time) *Commitment {
	if s == nil {
		return nil
	}
	if s.Commitments != nil {
		for i := range s.Commitments[providerID] {
			c := &s.Commitments[providerID][i]
			if (c.EffectiveAt == nil || !at.Before(*c.EffectiveAt)) && (c.EndsAt == nil || at.Before(*c.EndsAt)) {
				return c
			}
		}
		return nil
	}
	return s.Providers[providerID].CommitmentAt(at)
}
