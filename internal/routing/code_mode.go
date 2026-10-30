package routing

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func (s *Snapshot) CodeConnection(route codemode.Route, providerID string) (Configuration, bool) {
	configuration, ok := s.CodeConnections[route.RevisionID+":"+providerID]
	return configuration, ok
}

// codeRevision is what a published revision's frozen connections serve.
type codeRevision struct {
	adapters  []codemode.Adapter
	providers map[codeCapability][]string
	// vendors restrict models to the protocols their selected account serves.
	vendors map[string]codeadapter.Vendor
}

type codeCapability struct {
	path      string
	transport codeadapter.Transport
}

// CodeAdapters returns the adapters of a published route's frozen
// connections in table order. A route's pool may mix them.
func (s *Snapshot) CodeAdapters(route codemode.Route) []codemode.Adapter {
	return s.codeRevisions[route.RevisionID].adapters
}

// CodeProviders returns the providers of a published route's frozen
// connections whose adapter serves a client path over transport, ordered by ID.
// The returned slice is shared and must not be modified.
func (s *Snapshot) CodeProviders(route codemode.Route, path string, transport codeadapter.Transport) []string {
	if route.MessagesToCodex || route.ChatToAntigravity {
		return s.codeProvidersWithTranslation(route, path, transport)
	}
	return s.codeRevisions[route.RevisionID].providers[codeCapability{path, transport}]
}

func (s *Snapshot) codeProvidersWithTranslation(route codemode.Route, path string, transport codeadapter.Transport) []string {
	revision := s.codeRevisions[route.RevisionID]
	if transport != codeadapter.HTTP {
		return revision.providers[codeCapability{path, transport}]
	}
	return slices.Sorted(func(yield func(string) bool) {
		for provider, adapter := range revision.vendors {
			if endpoint, ok := codeadapter.RouteEndpoint(route, adapter, path); ok && endpoint.Supports(transport) {
				if !yield(provider) {
					return
				}
			}
		}
	})
}

// CodeModelProviders returns the providers of CodeProviders whose adapter
// serves a model on the path's protocol: an adapter that serves each model on
// one endpoint serves a path only for that endpoint's models.
func (s *Snapshot) CodeModelProviders(route codemode.Route, path string, model string, transport codeadapter.Transport) []string {
	resolved, err := route.ResolveModel(codemode.Operation{Model: model})
	if err != nil {
		return nil
	}
	revision := s.codeRevisions[route.RevisionID]
	providers := s.CodeProviders(route, path, transport)
	return slices.DeleteFunc(slices.Clone(providers), func(provider string) bool {
		adapter := revision.vendors[provider]
		endpoint, ok := codeadapter.RouteEndpoint(route, adapter, path)
		return !ok || !adapter.ServesModel(endpoint.Protocol, resolved.Model)
	})
}

func (s *Snapshot) validateCodeMode() error {
	if err := codeadapter.Validate(); err != nil {
		return err
	}
	for slug, route := range s.CodeRoutes {
		if err := route.ValidateAliases(); err != nil {
			return err
		}
		if route.InitialRecoveryAttempts < 0 || route.InitialRecoveryAttempts > 2 {
			return fmt.Errorf("invalid initial recovery policy for %s", slug)
		}
		if err := route.Tools.Validate(); err != nil {
			return err
		}
		if !bodylimit.Valid(route.MaxBodyBytes) {
			return fmt.Errorf("invalid code route body limit for %s", slug)
		}
		if route.Slug != slug || route.ID == "" || route.ProjectID == "" || route.PoolID == "" || route.RevisionID == "" || route.Revision < 1 || route.PublishedAt == nil {
			return fmt.Errorf("invalid code route %s", slug)
		}
		if err := codemode.ValidateModels(route.Models); err != nil {
			return err
		}
		if _, exists := s.Routes[slug]; exists {
			return fmt.Errorf("code route slug conflicts with ordinary route %s", slug)
		}
	}
	return nil
}

func (s *Snapshot) compileCodeMode() error {
	// Connections no adapter accepts serve nothing.
	s.codeRevisions = make(map[string]codeRevision)
	for key, c := range s.CodeConnections {
		vendor, ok := codeadapter.ForConnection(c.Kind, c.AuthMode, c.ProfileID)
		if !ok {
			continue
		}
		id, provider, _ := strings.Cut(key, ":")
		revision := s.codeRevisions[id]
		if !slices.Contains(revision.adapters, vendor.Adapter) {
			revision.adapters = append(revision.adapters, vendor.Adapter)
		}
		if revision.providers == nil {
			revision.providers = make(map[codeCapability][]string)
			revision.vendors = make(map[string]codeadapter.Vendor)
		}
		for _, endpoint := range vendor.Endpoints() {
			for _, transport := range []codeadapter.Transport{codeadapter.HTTP, codeadapter.WebSocket} {
				if endpoint.Target == "" && endpoint.Supports(transport) {
					capability := codeCapability{endpoint.ClientPath, transport}
					revision.providers[capability] = append(revision.providers[capability], provider)
				}
			}
		}
		revision.vendors[provider] = vendor
		s.codeRevisions[id] = revision
	}
	for _, revision := range s.codeRevisions {
		slices.SortFunc(revision.adapters, codeadapter.Compare)
		for _, providers := range revision.providers {
			slices.Sort(providers)
		}
	}
	return nil
}
