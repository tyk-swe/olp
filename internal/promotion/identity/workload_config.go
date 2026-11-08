package identity

import (
	"maps"
	"reflect"

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
		Algorithms:         promotion.Map(issuer.Algorithms, func(a contract.ConfigurationWorkloadIssuerAlgorithms) string { return string(a) }),
		MaxLifetimeSeconds: int64(issuer.MaxLifetimeSeconds),
		DisabledKeyIDs:     issuer.DisabledKeyIds,
		Mappings: promotion.Map(issuer.Mappings, func(m contract.ConfigurationWorkloadMapping) workload.Mapping {
			return workload.Mapping{
				AgentPrincipal:     promotion.ValueOf(m.AgentPrincipal),
				Name:               m.Name,
				Match:              m.Match,
				ProjectID:          projectID(m.Project),
				LimitTemplate:      m.LimitTemplate,
				RouteGroups:        m.RouteGroups,
				AllowedToolsets:    identifiers(m.AllowedToolsets),
				AllowedAgents:      identifiers(m.AllowedAgents),
				Scopes:             promotion.Map(m.Scopes, func(s contract.ConfigurationWorkloadMappingScopes) string { return string(s) }),
				EndUserClaim:       promotion.Pointer(m.EndUserClaim),
				AllowProviderState: promotion.ValueOf(m.AllowProviderState),
			}
		}),
	}
}

func entryOf(config workload.Config, directory project.Directory) (contract.ConfigurationWorkloadIssuer, error) {
	mappings, err := promotion.TryMap(config.Mappings, func(m workload.Mapping) (contract.ConfigurationWorkloadMapping, error) {
		return mappingEntry(m, directory)
	})
	if err != nil {
		return contract.ConfigurationWorkloadIssuer{}, err
	}
	return contract.ConfigurationWorkloadIssuer{
		Algorithms: promotion.Map(config.Algorithms, func(a string) contract.ConfigurationWorkloadIssuerAlgorithms {
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
		AgentPrincipal:     promotion.NonZero(m.AgentPrincipal),
		AllowProviderState: &m.AllowProviderState,
		AllowedAgents:      agents,
		AllowedToolsets:    toolsets,
		EndUserClaim:       promotion.Nullable(m.EndUserClaim),
		LimitTemplate:      m.LimitTemplate,
		Match:              m.Match,
		Name:               m.Name,
		Project:            name,
		RouteGroups:        m.RouteGroups,
		Scopes: promotion.Map(m.Scopes, func(s string) contract.ConfigurationWorkloadMappingScopes {
			return contract.ConfigurationWorkloadMappingScopes(s)
		}),
	}, nil
}

func sameConfig(a, b workload.Config) bool {
	return reflect.DeepEqual(canonicalConfig(a), canonicalConfig(b))
}

func canonicalConfig(c workload.Config) workload.Config {
	return workload.Config{
		Name:               c.Name,
		Issuer:             c.Issuer,
		JWKSURL:            c.JWKSURL,
		Enabled:            c.Enabled,
		Audiences:          promotion.NonNil(c.Audiences),
		Algorithms:         promotion.NonNil(c.Algorithms),
		MaxLifetimeSeconds: c.MaxLifetimeSeconds,
		DisabledKeyIDs:     promotion.NonNil(c.DisabledKeyIDs),
		Mappings:           promotion.Map(c.Mappings, canonicalMapping),
	}
}

func canonicalMapping(m workload.Mapping) workload.Mapping {
	return workload.Mapping{
		AgentPrincipal:     m.AgentPrincipal,
		Name:               m.Name,
		Match:              maps.Collect(maps.All(m.Match)),
		ProjectID:          m.ProjectID,
		LimitTemplate:      m.LimitTemplate,
		RouteGroups:        promotion.NonNil(m.RouteGroups),
		AllowedToolsets:    promotion.NonNil(m.AllowedToolsets),
		AllowedAgents:      promotion.NonNil(m.AllowedAgents),
		Scopes:             promotion.NonNil(m.Scopes),
		EndUserClaim:       m.EndUserClaim,
		AllowProviderState: m.AllowProviderState,
	}
}

func declaredProjects(projects []contract.ConfigurationProjectEntry) map[string]contract.ConfigurationProjectEntry {
	return promotion.Associate(projects, func(entry contract.ConfigurationProjectEntry) (string, contract.ConfigurationProjectEntry) {
		return promotion.Fold(entry.Name), entry
	})
}

func identifiers(values *[]openapi_types.UUID) []string {
	if values == nil {
		return nil
	}
	return promotion.Map(*values, openapi_types.UUID.String)
}

func uuids(values []string) (*[]openapi_types.UUID, error) {
	if len(values) == 0 {
		return nil, nil
	}
	parsed, err := promotion.Convert[[]openapi_types.UUID](values)
	return &parsed, err
}
