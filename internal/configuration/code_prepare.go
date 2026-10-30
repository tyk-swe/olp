package configuration

import (
	"context"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
)

func (s *Server) prepareCode(ctx context.Context, q access.Queryer, code *CodeConfiguration) (*CodeConfiguration, error) {
	if code == nil {
		return nil, nil
	}
	type connection struct {
		Name  string
		Entry CodeConnectionEntry
	}
	type paidConnection struct {
		Name  string
		Entry CodePaidConnection
	}
	routes, err := mapCodeResult(code.Routes, func(route CodeRouteEntry) (CodeRouteEntry, error) {
		if route.Published == nil {
			return route, nil
		}
		publication := route.Published
		connections, err := mapCodeResult(slices.Collect(maps.Keys(publication.Connections)), func(name string) (connection, error) {
			entry := publication.Connections[name]
			cfg := entry.Configuration
			if cfg.Options.Network != nil && cfg.Options.Network.CredentialID != "" {
				return connection{}, access.Invalid("code.connections", "Use a network credential reference instead of an installation credential ID.")
			}
			if entry.NetworkCredentialRef != nil && (*entry.NetworkCredentialRef != networkRef(name) || cfg.Options.Network == nil) {
				return connection{}, access.Invalid("code.connections", "Use the named provider network credential reference.")
			}
			refusal, err := pinPlugin(ctx, q, s.Unconfined, &cfg)
			if err != nil {
				return connection{}, err
			}
			if refusal != "" {
				return connection{}, access.Invalid("code.connections", "Install and approve the pinned frozen provider profile before promotion.")
			}
			cfg.Normalize()
			if err := cfg.Validate(s.Egress); err != nil {
				return connection{}, err
			}
			return connection{Name: name, Entry: CodeConnectionEntry{Configuration: cfg, NetworkCredentialRef: entry.NetworkCredentialRef}}, nil
		})
		if err != nil {
			return CodeRouteEntry{}, err
		}
		paid, err := mapCodeResult(slices.Collect(maps.Keys(publication.PaidConnections)), func(name string) (paidConnection, error) {
			entry := publication.PaidConnections[name]
			document := &Document{APIVersion: APIVersion, Providers: []ProviderEntry{entry.Provider}}
			if _, err := s.validateDocument(ctx, q, document); err != nil {
				return paidConnection{}, err
			}
			return paidConnection{Name: name, Entry: CodePaidConnection{Provider: document.Providers[0], SlotKeyRefs: entry.SlotKeyRefs}}, nil
		})
		if err != nil {
			return CodeRouteEntry{}, err
		}
		return CodeRouteEntry{ID: route.ID, Project: route.Project, Slug: route.Slug, Draft: route.Draft, Published: &CodePublication{Policy: publication.Policy, Connections: maps.Collect(func(yield func(string, CodeConnectionEntry) bool) {
			for _, connection := range connections {
				if !yield(connection.Name, connection.Entry) {
					return
				}
			}
		}), PaidConnections: maps.Collect(func(yield func(string, CodePaidConnection) bool) {
			for _, connection := range paid {
				if !yield(connection.Name, connection.Entry) {
					return
				}
			}
		})}}, nil
	})
	if err != nil {
		return nil, err
	}
	return &CodeConfiguration{Accounts: code.Accounts, Pools: code.Pools, Routes: routes, Budgets: code.Budgets}, nil
}
