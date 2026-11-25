package keyauthority

import "github.com/tyk-swe/olp/internal/workload"

func WorkloadPolicy(m workload.Mapping) KeyPolicy {
	template := m.LimitTemplate
	return KeyPolicy{Scopes: m.Scopes, AllowedToolsets: m.AllowedToolsets, AllowedAgents: m.AllowedAgents, AllowedRoutes: []string{}, AllowedAttributionKeys: []string{}, AllowedRouteGroups: m.RouteGroups, LimitTemplate: &template, AllowProviderState: m.AllowProviderState}
}
