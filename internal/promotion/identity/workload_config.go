package identity

import (
	"maps"
	"slices"

	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/workload"
)

func configOf(issuer contract.ConfigurationWorkloadIssuer, projectID func(string) string) workload.Config {
	return workload.Config{
		Name:               issuer.Name,
		Issuer:             issuer.Issuer,
		JWKSURL:            issuer.JwksUrl,
		Enabled:            issuer.Enabled,
		Audiences:          issuer.Audiences,
		Algorithms:         mapped(issuer.Algorithms, func(a contract.ConfigurationWorkloadIssuerAlgorithms) string { return string(a) }),
		MaxLifetimeSeconds: int64(issuer.MaxLifetimeSeconds),
		DisabledKeyIDs:     issuer.DisabledKeyIds,
		Mappings: mapped(issuer.Mappings, func(m contract.ConfigurationWorkloadMapping) workload.Mapping {
			return workload.Mapping{
				AgentPrincipal:     m.AgentPrincipal != nil && *m.AgentPrincipal,
				Name:               m.Name,
				Match:              m.Match,
				ProjectID:          projectID(m.Project),
				LimitTemplate:      m.LimitTemplate,
				RouteGroups:        m.RouteGroups,
				AllowedToolsets:    identifiers(m.AllowedToolsets),
				AllowedAgents:      identifiers(m.AllowedAgents),
				Scopes:             mapped(m.Scopes, func(s contract.ConfigurationWorkloadMappingScopes) string { return string(s) }),
				EndUserClaim:       optional(m.EndUserClaim),
				AllowProviderState: m.AllowProviderState != nil && *m.AllowProviderState,
			}
		}),
	}
}

func entryOf(config workload.Config, directory project.Directory) (contract.ConfigurationWorkloadIssuer, error) {
	mappings, err := collected(config.Mappings, func(m workload.Mapping) (contract.ConfigurationWorkloadMapping, error) {
		return mappingEntry(m, directory)
	})
	if err != nil {
		return contract.ConfigurationWorkloadIssuer{}, err
	}
	return contract.ConfigurationWorkloadIssuer{
		Algorithms: mapped(config.Algorithms, func(a string) contract.ConfigurationWorkloadIssuerAlgorithms {
			return contract.ConfigurationWorkloadIssuerAlgorithms(a)
		}),
		Audiences:          config.Audiences,
		DisabledKeyIds:     config.DisabledKeyIDs,
		Enabled:            config.Enabled,
		Issuer:             config.Issuer,
		JwksUrl:            config.JWKSURL,
		Mappings:           mappings,
		MaxLifetimeSeconds: int(config.MaxLifetimeSeconds),
		Name:               config.Name,
	}, nil
}

func mappingEntry(m workload.Mapping, directory project.Directory) (contract.ConfigurationWorkloadMapping, error) {
	name, ok := directory.Name(m.ProjectID)
	if !ok {
		return contract.ConfigurationWorkloadMapping{}, access.Invalid("workload_issuers", "A workload mapping project is no longer available.")
	}
	toolsets, err := uuids(m.AllowedToolsets)
	if err != nil {
		return contract.ConfigurationWorkloadMapping{}, err
	}
	agents, err := uuids(m.AllowedAgents)
	if err != nil {
		return contract.ConfigurationWorkloadMapping{}, err
	}
	return contract.ConfigurationWorkloadMapping{
		AgentPrincipal:     enabled(m.AgentPrincipal),
		AllowProviderState: &m.AllowProviderState,
		AllowedAgents:      agents,
		AllowedToolsets:    toolsets,
		EndUserClaim:       promotion.Nullable(m.EndUserClaim),
		LimitTemplate:      m.LimitTemplate,
		Match:              m.Match,
		Name:               m.Name,
		Project:            name,
		RouteGroups:        m.RouteGroups,
		Scopes: mapped(m.Scopes, func(s string) contract.ConfigurationWorkloadMappingScopes {
			return contract.ConfigurationWorkloadMappingScopes(s)
		}),
	}, nil
}

func sameConfig(a, b workload.Config) bool {
	return a.Name == b.Name &&
		a.Issuer == b.Issuer &&
		a.JWKSURL == b.JWKSURL &&
		a.Enabled == b.Enabled &&
		slices.Equal(a.Audiences, b.Audiences) &&
		slices.Equal(a.Algorithms, b.Algorithms) &&
		a.MaxLifetimeSeconds == b.MaxLifetimeSeconds &&
		slices.Equal(a.DisabledKeyIDs, b.DisabledKeyIDs) &&
		slices.EqualFunc(a.Mappings, b.Mappings, sameMapping)
}

func sameMapping(a, b workload.Mapping) bool {
	return a.AgentPrincipal == b.AgentPrincipal &&
		a.Name == b.Name &&
		maps.Equal(a.Match, b.Match) &&
		a.ProjectID == b.ProjectID &&
		a.LimitTemplate == b.LimitTemplate &&
		slices.Equal(a.RouteGroups, b.RouteGroups) &&
		slices.Equal(a.AllowedToolsets, b.AllowedToolsets) &&
		slices.Equal(a.AllowedAgents, b.AllowedAgents) &&
		slices.Equal(a.Scopes, b.Scopes) &&
		samePointer(a.EndUserClaim, b.EndUserClaim) &&
		a.AllowProviderState == b.AllowProviderState
}

func declaredProjects(projects []contract.ConfigurationProjectEntry) map[string]contract.ConfigurationProjectEntry {
	return indexed(projects, func(entry contract.ConfigurationProjectEntry) (string, contract.ConfigurationProjectEntry) {
		return folded(entry.Name), entry
	})
}

func identifiers(values *[]openapi_types.UUID) []string {
	if values == nil {
		return nil
	}
	return mapped(*values, openapi_types.UUID.String)
}

func uuids(values []string) (*[]openapi_types.UUID, error) {
	if len(values) == 0 {
		return nil, nil
	}
	parsed, err := promotion.Convert[[]openapi_types.UUID](values)
	return &parsed, err
}

func enabled(flag bool) *bool {
	if !flag {
		return nil
	}
	return &flag
}

func optional[T any](value nullable.Nullable[T]) *T {
	present, ok := promotion.Present(value)
	if !ok {
		return nil
	}
	return &present
}

func samePointer[T comparable](a, b *T) bool {
	return a == b || a != nil && b != nil && *a == *b
}
