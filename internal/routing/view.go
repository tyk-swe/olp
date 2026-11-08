package routing

import (
	"iter"
	"maps"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/durablecontract"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/mediacontract"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/realtimecontract"
)

type View struct{ snapshot *Snapshot }

func (s *Snapshot) ReadOnly() *View {
	if s == nil {
		return nil
	}
	owned := s.Clone()
	owned.mcpTools = s.mcpTools
	owned.guardrailEngines = s.guardrailEngines
	owned.toolGuardrailEngines = s.toolGuardrailEngines
	owned.hasBodyLimits = s.hasBodyLimits
	owned.codeRevisions = s.codeRevisions
	owned.interactions = s.interactions
	owned.operations = s.operations
	owned.media = s.media
	owned.durable = s.durable
	owned.realtime = s.realtime
	owned.webrtc = s.webrtc
	owned.duplex = s.duplex
	for id, provider := range owned.Providers {
		provider.metadata = s.Providers[id].metadata
		owned.Providers[id] = provider
	}
	for slug, route := range owned.Routes {
		route.compiledContentPolicy = s.Routes[slug].compiledContentPolicy.Clone()
		owned.Routes[slug] = route
	}
	return &View{snapshot: owned}
}

func (v *View) Clone() *Snapshot       { return v.snapshot.Clone() }
func (v *View) Generation() Generation { return v.snapshot.Generation }
func (v *View) ProviderCount() int     { return len(v.snapshot.Providers) }
func (v *View) RouteCount() int        { return len(v.snapshot.Routes) }
func (v *View) RouteSlugs() []string   { return slices.Sorted(maps.Keys(v.snapshot.Routes)) }
func (v *View) ProviderIDs() []string  { return slices.Sorted(maps.Keys(v.snapshot.Providers)) }
func (v *View) HasBodyLimits() bool    { return v != nil && v.snapshot.HasBodyLimits() }

func (v *View) FindRoute(slug string) (Route, bool) {
	route, ok := v.snapshot.Routes[slug]
	return detachedRoute(route), ok
}
func (v *View) Route(slug string) Route { route, _ := v.FindRoute(slug); return route }
func (v *View) FindProvider(id string) (Provider, bool) {
	provider, ok := v.snapshot.Providers[id]
	return detachedProvider(provider), ok
}
func (v *View) Provider(id string) Provider { provider, _ := v.FindProvider(id); return provider }
func (v *View) Routes() iter.Seq2[string, Route] {
	return func(yield func(string, Route) bool) {
		for slug := range v.snapshot.Routes {
			if !yield(slug, v.Route(slug)) {
				return
			}
		}
	}
}
func (v *View) Providers() iter.Seq2[string, Provider] {
	return func(yield func(string, Provider) bool) {
		for id := range v.snapshot.Providers {
			if !yield(id, v.Provider(id)) {
				return
			}
		}
	}
}
func (v *View) InstallationPolicy() *Policy { return v.snapshot.InstallationPolicy.Clone() }
func (v *View) KeyPolicy(id string) *Policy { return v.snapshot.KeyPolicies[id].Clone() }
func (v *View) AgentPublications() []agentcontract.Revision {
	return slices.Collect(func(yield func(agentcontract.Revision) bool) {
		for _, revision := range v.snapshot.AgentPublications {
			if !yield(agentcontract.CloneRevision(revision)) {
				return
			}
		}
	})
}
func (v *View) FindCodeRoute(slug string) (codemode.Route, bool) {
	route, ok := v.snapshot.CodeRoutes[slug]
	return cloneCodeRoute(route), ok
}
func (v *View) CodeRoutes() iter.Seq2[string, codemode.Route] {
	return func(yield func(string, codemode.Route) bool) {
		for slug, route := range v.snapshot.CodeRoutes {
			if !yield(slug, cloneCodeRoute(route)) {
				return
			}
		}
	}
}
func (v *View) FindCodePaidConnection(id string) (Provider, bool) {
	provider, ok := v.snapshot.CodePaidConnections[id]
	return cloneProvider(provider), ok
}
func (v *View) CodeConnection(route codemode.Route, providerID string) (Configuration, bool) {
	configuration, ok := v.snapshot.CodeConnection(route, providerID)
	return configuration.Clone(), ok
}
func (v *View) CommitmentAt(providerID string, at time.Time) *Commitment {
	commitment := v.snapshot.CommitmentAt(providerID, at)
	if commitment == nil {
		return nil
	}
	return &Commitment{ID: commitment.ID, Amount: commitment.Amount, Period: commitment.Period, EffectiveAt: copyPointer(commitment.EffectiveAt), EndsAt: copyPointer(commitment.EndsAt)}
}
func (v *View) GuardrailEngine(slug, key string) *guardrails.Engine {
	return copyPointer(v.snapshot.GuardrailEngine(slug, key))
}
func (v *View) ToolGuardrailEngine(toolsetID, key string) *guardrails.Engine {
	return copyPointer(v.snapshot.ToolGuardrailEngine(toolsetID, key))
}
func (v *View) MCPToolset(revisionID string) (MCPToolsetResolution, bool) {
	return v.snapshot.MCPToolset(revisionID)
}
func (v *View) HasCodeAdapters(route codemode.Route) bool { return v.snapshot.HasCodeAdapters(route) }
func (v *View) CodeProviders(route codemode.Route, path string, transport codeadapter.Transport) []string {
	return v.snapshot.CodeProviders(route, path, transport)
}
func (v *View) HasCodeProviders(route codemode.Route, path string, transport codeadapter.Transport) bool {
	return v.snapshot.HasCodeProviders(route, path, transport)
}
func (v *View) CodeModelProviders(route codemode.Route, path, model string, transport codeadapter.Transport) []string {
	return v.snapshot.CodeModelProviders(route, path, model, transport)
}

func (v *View) PinnedCurrent(route Route, provider Provider, target Target, slot Slot, keyID, surface, mode string) (*View, Slot, bool) {
	pinned, selected, ok := v.snapshot.PinnedCurrent(route, provider, target, slot, keyID, surface, mode)
	if !ok {
		return nil, Slot{}, false
	}
	return &View{snapshot: pinned}, cloneSlot(selected), true
}
func (v *View) Historical(provider Provider, route Route) (*View, error) {
	snapshot := &Snapshot{
		Generation:         v.snapshot.Generation,
		Providers:          map[string]Provider{provider.ID: cloneProvider(provider)},
		Routes:             map[string]Route{route.Slug: cloneRoute(route)},
		InstallationPolicy: v.snapshot.InstallationPolicy,
		KeyPolicies:        v.snapshot.KeyPolicies,
		Guardrails:         v.snapshot.Guardrails,
	}
	if err := snapshot.Compile(); err != nil {
		return nil, err
	}
	return &View{snapshot: snapshot}, nil
}

type selectionSource interface {
	lookupRoute(string) (Route, bool)
	lookupProvider(string) (Provider, bool)
	selectionPolicies(string) (*Policy, *Policy)
}

func (s *Snapshot) lookupRoute(slug string) (Route, bool) {
	route, ok := s.Routes[slug]
	return route, ok
}
func (s *Snapshot) lookupProvider(id string) (Provider, bool) {
	provider, ok := s.Providers[id]
	return provider, ok
}
func (s *Snapshot) selectionPolicies(key string) (*Policy, *Policy) {
	return s.InstallationPolicy, s.KeyPolicies[key]
}
func (v *View) lookupRoute(slug string) (Route, bool)     { return v.snapshot.lookupRoute(slug) }
func (v *View) lookupProvider(id string) (Provider, bool) { return v.snapshot.lookupProvider(id) }
func (v *View) selectionPolicies(key string) (*Policy, *Policy) {
	return v.InstallationPolicy(), v.KeyPolicy(key)
}

func (v *View) InteractionTemplate(slug, target string) (*interaction.Template, bool) {
	template, ok := v.snapshot.InteractionTemplate(slug, target)
	return copyPointer(template), ok
}

func (v *View) RealtimeTemplate(slug, target string) (*realtimecontract.Template, bool) {
	template, ok := v.snapshot.RealtimeTemplate(slug, target)
	return copyPointer(template), ok
}

func (v *View) WebRTCTemplate(slug, target string) (*realtimecontract.Template, bool) {
	template, ok := v.snapshot.WebRTCTemplate(slug, target)
	return copyPointer(template), ok
}

func (v *View) DuplexTemplate(slug, target string) (*duplexcontract.Template, bool) {
	template, ok := v.snapshot.DuplexTemplate(slug, target)
	return copyPointer(template), ok
}

func (v *View) MediaTemplate(slug, target, operation string) (*mediacontract.Template, bool) {
	template, ok := v.snapshot.MediaTemplate(slug, target, operation)
	return copyPointer(template), ok
}

func (v *View) DurableTemplate(slug, target string) (*durablecontract.Template, bool) {
	template, ok := v.snapshot.DurableTemplate(slug, target)
	return copyPointer(template), ok
}

func (v *View) OperationTemplate(slug, target, operation string) (*operationplan.Template, bool) {
	template, ok := v.snapshot.OperationTemplate(slug, target, operation)
	return copyPointer(template), ok
}

func (v *View) WithRoute(route Route) *View {
	snapshot := *v.snapshot
	snapshot.Routes = map[string]Route{route.Slug: cloneRoute(route)}
	return &View{snapshot: &snapshot}
}

func (v *View) HasGuardrails() bool              { return v.snapshot.Guardrails != nil }
func (v *View) Guardrails() *guardrails.Registry { return v.snapshot.Guardrails.Clone() }
func (v *View) GuardrailWindow() int             { return v.snapshot.Guardrails.Window() }
func (v *View) GuardrailDefinition(revision string) guardrails.Revision {
	definition := v.snapshot.Guardrails.Definitions[revision]
	definition.Configuration = slices.Clone(definition.Configuration)
	return definition
}
func (v *View) GuardrailEntries(project, route, key string) ([]guardrails.Entry, error) {
	registry := v.snapshot.Guardrails
	if registry == nil {
		return nil, nil
	}
	return registry.Entries(guardrails.Boundary{ProjectID: project, OrganizationID: registry.ProjectOrganizations[project], RouteID: route, KeyID: key})
}

func (s *Snapshot) CompiledView() (*View, error) {
	owned := s.Clone()
	if err := owned.Compile(); err != nil {
		return nil, err
	}
	return &View{snapshot: owned}, nil
}

func (v *View) Digest() (string, error) { return v.snapshot.Digest() }
