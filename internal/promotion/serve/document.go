package serve

import (
	"context"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/agentpublication"
	"github.com/tyk-swe/olp/internal/promotion/code"
	"github.com/tyk-swe/olp/internal/promotion/guardrail"
	"github.com/tyk-swe/olp/internal/promotion/identity"
	"github.com/tyk-swe/olp/internal/promotion/installation"
	"github.com/tyk-swe/olp/internal/promotion/mcpserver"
	"github.com/tyk-swe/olp/internal/promotion/organization"
	"github.com/tyk-swe/olp/internal/promotion/pricing"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/internal/promotion/retrievalbinding"
	"github.com/tyk-swe/olp/internal/promotion/route"
	"github.com/tyk-swe/olp/internal/promotion/sink"
)

type snapshot struct {
	installation  installation.Settings
	organizations []organization.Organization
	projects      []project.Project
	identity      identity.Desired
	providers     []provider.Entry
	routes        route.Configuration
	pricing       pricing.Desired
	sinks         []sink.Sink
	guardrails    guardrail.Section
	servers       []mcpserver.Server
	retrieval     []retrievalbinding.Binding
	publications  []agentpublication.Publication
	code          code.Code
}

type sections struct {
	installation  installation.Settings
	organizations []organization.Organization
	projects      []project.Project
	identity      identity.Desired
	providers     []provider.Entry
	routes        route.Desired
	pricing       pricing.Desired
	sinks         []sink.Sink
	guardrails    guardrail.Section
	servers       []mcpserver.Server
	retrieval     retrievalbinding.Desired
	publications  []agentpublication.Publication
	code          code.Desired
}

func (s sections) snapshot() snapshot {
	return snapshot{
		installation:  s.installation,
		organizations: s.organizations,
		projects:      s.projects,
		identity:      s.identity,
		providers:     s.providers,
		routes:        s.routes.Configuration,
		pricing:       s.pricing,
		sinks:         s.sinks,
		guardrails:    s.guardrails,
		servers:       s.servers,
		retrieval:     s.retrieval.Bindings,
		publications:  s.publications,
		code:          s.code.Code,
	}
}

func (s server) parse(document contract.ConfigurationDocument, declared *contract.ConfigurationCodeBindings) (sections, error) {
	names := promotion.NamesOf(document)
	settings, err := installation.Parse(document)
	if err != nil {
		return sections{}, err
	}
	organizations, err := organization.Parse(document)
	if err != nil {
		return sections{}, err
	}
	projects, err := project.Parse(document)
	if err != nil {
		return sections{}, err
	}
	identities, err := identity.Parse(document)
	if err != nil {
		return sections{}, err
	}
	providers, err := provider.ParseAll(document.Providers, names, s.providers)
	if err != nil {
		return sections{}, err
	}
	routes, err := route.Parse(document)
	if err != nil {
		return sections{}, err
	}
	prices, err := pricing.Parse(document)
	if err != nil {
		return sections{}, err
	}
	sinks, err := sink.Parse(document, s.egress)
	if err != nil {
		return sections{}, err
	}
	guardrails, err := guardrail.Parse(document)
	if err != nil {
		return sections{}, err
	}
	servers, err := mcpserver.Parse(document, s.egress)
	if err != nil {
		return sections{}, err
	}
	retrieval, err := retrievalbinding.Parse(document, names)
	if err != nil {
		return sections{}, err
	}
	publications, err := agentpublication.Parse(document, s.egress)
	if err != nil {
		return sections{}, err
	}
	codes, err := code.Parse(document.Code, declared, names, s.providers)
	if err != nil {
		return sections{}, err
	}
	return sections{
		installation:  settings,
		organizations: organizations,
		projects:      projects,
		identity:      identities,
		providers:     providers,
		routes:        routes,
		pricing:       prices,
		sinks:         sinks,
		guardrails:    guardrails,
		servers:       servers,
		retrieval:     retrieval,
		publications:  publications,
		code:          codes,
	}, nil
}

func read(ctx context.Context, q access.Queryer) (snapshot, error) {
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	settings, err := installation.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	organizations, err := organization.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	projects, err := project.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	identities, err := identity.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	providers, err := provider.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	routes, err := route.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	prices, err := pricing.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	sinks, err := sink.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	guardrails, err := guardrail.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	servers, err := mcpserver.Read(ctx, q, directory)
	if err != nil {
		return snapshot{}, err
	}
	retrieval, err := retrievalbinding.Read(ctx, q, directory)
	if err != nil {
		return snapshot{}, err
	}
	publications, err := agentpublication.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	codes, err := code.Read(ctx, q)
	if err != nil {
		return snapshot{}, err
	}
	return snapshot{
		installation:  settings,
		organizations: organizations,
		projects:      projects.Projects(),
		identity:      identities,
		providers:     providers,
		routes:        routes.Configuration,
		pricing:       prices,
		sinks:         sinks,
		guardrails:    guardrails,
		servers:       servers,
		retrieval:     retrieval,
		publications:  publications,
		code:          codes,
	}, nil
}

func (s snapshot) render(exportedAt *time.Time) (contract.ConfigurationDocument, error) {
	settings, err := installation.Render(s.installation)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	organizations, err := organization.Render(s.organizations)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	projects, err := project.Render(s.projects)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	providers, err := provider.Render(s.providers)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	routes, templates, err := route.Render(s.routes)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	guardrails, err := guardrail.Render(s.guardrails)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	publications, err := agentpublication.Render(s.publications)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	codes, err := code.Render(s.code)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	identities, err := identity.Render(s.identity)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	prices := pricing.Render(s.pricing)
	return contract.ConfigurationDocument{
		ApiVersion:           contract.OpenllmproxyDevconfigv2,
		ExportedAt:           exportedAt,
		BudgetTimeZone:       settings.BudgetTimeZone,
		InstallationBudget:   settings.InstallationBudget,
		RequireLocalMfa:      settings.RequireLocalMfa,
		Organizations:        organizations,
		Projects:             projects,
		Saml:                 identities.Saml,
		ScimGroupMappings:    identities.ScimGroupMappings,
		WorkloadIssuers:      identities.WorkloadIssuers,
		Providers:            providers,
		Routes:               routes,
		Templates:            templates,
		Pricing:              prices.Pricing,
		Discounts:            prices.Discounts,
		RateCards:            prices.RateCards,
		Sinks:                sink.Render(s.sinks),
		Guardrails:           guardrails.Guardrails,
		GuardrailPolicies:    guardrails.GuardrailPolicies,
		GuardrailAttachments: guardrails.GuardrailAttachments,
		McpServers:           mcpserver.Render(s.servers),
		RetrievalBindings:    retrievalbinding.Render(s.retrieval),
		AgentPublications:    publications,
		Code:                 codes,
	}, nil
}
