package serve

import (
	"context"
	"net/http"
	"slices"

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
		code.Planner(d.code, s.providers),
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
		pricing.Applier(d.pricing, s.normalizer),
		route.GroupApplier(d.routes),
		sink.Applier(d.sinks, d.bindings, s.sinks),
		guardrail.CredentialApplier(d.guardrails, d.bindings, s.vault),
		guardrail.GuardrailApplier(d.guardrails),
		guardrail.PolicyApplier(d.guardrails),
		project.CatalogApplier(d.projects),
		code.Applier(d.code, s.providers),
		mcpserver.Applier(d.servers, d.bindings, ready.servers, s.servers.Keys),
		retrievalbinding.Applier(d.retrieval),
		agentpublication.Applier(d.publications, d.bindings, s.publications),
		guardrail.ToolsetApplier(d.guardrails),
		identity.WorkloadApplier(d.identity, ready.workload, s.identity),
		project.TemplateApplier(d.projects),
		identity.SAMLApplier(d.identity, s.identity, r),
	}
}

func evaluate(ctx context.Context, q access.Queryer, d desired) (contract.ConfigurationPlanResponse, error) {
	items, err := planned(ctx, q, d.planners)
	if err != nil {
		return contract.ConfigurationPlanResponse{}, err
	}
	drift, err := drifted(ctx, q, d.expected)
	if err != nil {
		return contract.ConfigurationPlanResponse{}, err
	}
	return promotion.Result(d.digest, slices.Concat(items, drift)), nil
}

func planned(ctx context.Context, q access.Queryer, planners []promotion.Planner) ([]promotion.Item, error) {
	batches, err := promotion.TryMap(planners, func(planner promotion.Planner) ([]promotion.Item, error) {
		if planner.Plan == nil {
			return nil, nil
		}
		return planner.Plan(ctx, q)
	})
	if err != nil {
		return nil, err
	}
	return slices.Concat(batches...), nil
}

func drifted(ctx context.Context, q access.Queryer, expected *string) ([]promotion.Item, error) {
	if expected == nil {
		return nil, nil
	}
	current, err := read(ctx, q)
	if err != nil {
		return nil, err
	}
	document, err := current.render(nil)
	if err != nil {
		return nil, err
	}
	digest, err := promotion.Digest(document)
	if err != nil {
		return nil, err
	}
	if digest == *expected {
		return nil, nil
	}
	return []promotion.Item{promotion.Conflict(contract.ConfigurationPlanItemKindConfiguration, "expected_digest", "configuration_changed")}, nil
}
