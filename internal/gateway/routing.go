package gateway

import (
	"context"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/usage"
)

// selectionOptions supplies the shared routing authority and measurements.
// Each operation adds its own source demand and semantic preparation.
func (s *Server) selectionOptions(ctx context.Context, x *call) routing.Options {
	inputs := s.Runtime.RoutingInputs()
	x.routingInputs = inputs
	options := routing.Options{
		Region: s.cfg.Region,
		KeyID:  x.keyID, Preferences: x.legPreferences(),
		Inputs: inputs, Now: s.now(),
		CheckSlots: true, CredentialEligibility: s.Runtime.Eligibility,
		UnconfinedPlugins: s.cfg.UnconfinedPlugins, Context: ctx,
	}
	if s.Admission.ready() {
		options.Supply = s.Admission
	}
	if s.fleet.marked() {
		options.Unhealthy = s.unhealthy
	}
	options.Mirror = x.mirrorSeed()
	options.WorstCost = func(provider routing.Provider, target routing.Target, price *usage.RoutingPrice, discount *usage.RoutingDiscount) (usage.Cost, bool) {
		return x.targetCostBound(provider, target, price, discount)
	}
	// Only a route with selectors pays to describe the request to them.
	if x.route != nil && len(x.route.Selectors) > 0 {
		options.Features = x.features()
		options.Evaluate = s.evaluator(ctx, x, options.Features)
		if x.authorize != nil || x.fixed {
			options.Permitted = func(route routing.Route) bool {
				return !x.fixed && (x.authorize == nil || x.authorize(&route) == nil)
			}
		}
	}
	return options
}

// features describe the caller's request to route selectors, from what
// admission already computed: its estimated size, whether it streams, and the
// tools, modalities, structured output and reasoning effort it asks for.
// Attribution labels never take part.
func (x *call) features() *routing.Features {
	if x.generation.parsed == nil {
		return routing.DescribeRequest(x.operationName(), nil, nil)
	}
	return routing.DescribeRequest(x.operationName(), x.generation.parsed, x.sourceDemand(""))
}
