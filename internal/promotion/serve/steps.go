package serve

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/agentpublication"
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

type prepared struct {
	workload identity.Prepared
	servers  mcpserver.Prepared
}

func (s server) planners(d sections, names promotion.Names, bindings promotion.Bindings) []promotion.Planner {
	return []promotion.Planner{
		installation.Planner(d.installation),
		organization.Planner(d.organizations),
		project.Planner(d.projects, names),
		identity.Planner(d.identity),
		provider.Planner(d.providers, bindings, s.providers),
		route.Planner(d.routes, names),
		pricing.Planner(d.pricing, names),
		sink.Planner(d.sinks, names, bindings, s.sinks),
		guardrail.Planner(d.guardrails, names),
		mcpserver.Planner(d.servers, names, bindings, s.servers.Keys),
		retrievalbinding.Planner(d.retrieval),
		agentpublication.Planner(d.publications, names, bindings),
	}
}

func (s server) appliers(d desired, ready prepared, r *http.Request) []promotion.Applier {
	return []promotion.Applier{
		installation.MFAApplier(d.installation),
		organization.Applier(d.organizations),
		installation.TimeZoneApplier(d.installation),
		installation.BudgetApplier(d.installation),
		project.Applier(d.projects, s.costs),
		identity.SCIMApplier(d.identity, s.identity),
		provider.Applier(d.providers, d.bindings, s.providers),
		route.Applier(d.routes),
		route.TemplateApplier(d.routes),
		pricing.Applier(d.pricing, s.vendorKind),
		route.GroupApplier(d.routes),
		sink.Applier(d.sinks, d.bindings, s.sinks),
		guardrail.CredentialApplier(d.guardrails, d.bindings, s.vault),
		guardrail.GuardrailApplier(d.guardrails),
		guardrail.PolicyApplier(d.guardrails),
		project.CatalogApplier(d.projects),
		mcpserver.Applier(d.servers, d.bindings, ready.servers, s.servers.Keys),
		retrievalbinding.Applier(d.retrieval),
		agentpublication.Applier(d.publications, d.bindings, s.publications),
		guardrail.ToolsetApplier(d.guardrails),
		identity.WorkloadApplier(d.identity, ready.workload, s.identity),
		project.TemplateApplier(d.projects),
		identity.SAMLApplier(d.identity, s.identity, r),
	}
}
