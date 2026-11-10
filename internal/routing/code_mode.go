package routing

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
)

func (s *Snapshot) CodeConnection(route code.Route, providerID string) (Configuration, bool) {
	configuration, ok := s.CodeConnections[route.RevisionID+":"+providerID]
	return configuration, ok
}

func (s *Snapshot) CodePaidConnection(route code.Route, providerID string) (Provider, bool) {
	provider, ok := s.CodePaidConnections[route.RevisionID+":"+providerID]
	return provider, ok
}

type codeRevision struct {
	adapters  []code.Adapter
	providers map[codeCapability][]string
	vendors   map[string]vendor.Vendor
}

type codeCapability struct {
	path      string
	transport code.Transport
}

func (s *Snapshot) HasCodeAdapters(route code.Route) bool {
	return len(s.codeRevisions[route.RevisionID].adapters) != 0
}

func (s *Snapshot) CodeProviders(route code.Route, path string, transport code.Transport) []string {
	return slices.Clone(s.codeRevisions[route.RevisionID].providers[codeCapability{path, transport}])
}

func (s *Snapshot) HasCodeProviders(route code.Route, path string, transport code.Transport) bool {
	return len(s.codeRevisions[route.RevisionID].providers[codeCapability{path, transport}]) != 0
}

func (s *Snapshot) codeProvidersWithTranslation(route code.Route, path string) []string {
	revision := s.codeRevisions[route.RevisionID]
	return slices.Sorted(func(yield func(string) bool) {
		for provider, adapter := range revision.vendors {
			if endpoint, ok := vendor.RouteEndpoint(route, adapter, path); ok && endpoint.Supports(code.HTTP) {
				if !yield(provider) {
					return
				}
			}
		}
	})
}

func (s *Snapshot) CodeModelProviders(route code.Route, path, model string, transport code.Transport) []string {
	resolved, err := route.ResolveModel(code.Operation{Model: model})
	if err != nil {
		return nil
	}
	revision := s.codeRevisions[route.RevisionID]
	providers := s.CodeProviders(route, path, transport)
	return slices.DeleteFunc(providers, func(provider string) bool {
		adapter := revision.vendors[provider]
		endpoint, ok := vendor.RouteEndpoint(route, adapter, path)
		return !ok || !adapter.ServesModel(endpoint.Protocol, resolved.Model)
	})
}

func (s *Snapshot) validateCodeMode() error {
	if err := vendor.Validate(); err != nil {
		return err
	}
	for slug, route := range s.CodeRoutes {
		if err := route.Validate(); err != nil {
			return err
		}
		if route.Slug != slug || route.ID == "" || route.ProjectID == "" || route.PoolID == "" || route.RevisionID == "" || route.Revision < 1 || route.PublishedAt == nil {
			return fmt.Errorf("invalid code route %s", slug)
		}
		if _, exists := s.Routes[slug]; exists {
			return fmt.Errorf("code route slug conflicts with ordinary route %s", slug)
		}
	}
	return nil
}

func (s *Snapshot) compileCodeMode() error {
	s.codeRevisions = make(map[string]codeRevision)
	for key, c := range s.CodeConnections {
		v, ok := vendor.ForConnection(c.Kind, c.AuthMode, c.ProfileID)
		if !ok {
			continue
		}
		id, provider, _ := strings.Cut(key, ":")
		revision := s.codeRevisions[id]
		if !slices.Contains(revision.adapters, v.Adapter) {
			revision.adapters = append(revision.adapters, v.Adapter)
		}
		if revision.providers == nil {
			revision.providers = make(map[codeCapability][]string)
			revision.vendors = make(map[string]vendor.Vendor)
		}
		for _, endpoint := range v.Endpoints() {
			for _, transport := range []code.Transport{code.HTTP, code.WebSocket} {
				if endpoint.Target == "" && endpoint.Supports(transport) {
					capability := codeCapability{endpoint.ClientPath, transport}
					revision.providers[capability] = append(revision.providers[capability], provider)
				}
			}
		}
		revision.vendors[provider] = v
		s.codeRevisions[id] = revision
	}
	for _, revision := range s.codeRevisions {
		slices.SortFunc(revision.adapters, vendor.Compare)
		for _, providers := range revision.providers {
			slices.Sort(providers)
		}
	}
	for _, route := range s.CodeRoutes {
		revision, ok := s.codeRevisions[route.RevisionID]
		if !ok {
			continue
		}
		if route.MessagesToCodex {
			revision.providers[codeCapability{"v1/messages", code.HTTP}] = s.codeProvidersWithTranslation(route, "v1/messages")
		}
		if route.ChatToAntigravity || route.ChatToCodex {
			revision.providers[codeCapability{"v1/chat/completions", code.HTTP}] = s.codeProvidersWithTranslation(route, "v1/chat/completions")
		}
	}
	return nil
}
