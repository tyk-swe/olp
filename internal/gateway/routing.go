package gateway

import (
	"context"

	"github.com/tyk-swe/olp/internal/runtime"
)

// selectionOptions supplies the shared routing authority and measurements.
// Each operation adds its own source demand and semantic preparation.
func (s *Server) selectionOptions(ctx context.Context, x *execution) runtime.SelectionOptions {
	options := runtime.SelectionOptions{
		KeyID: x.keyID, Preferences: x.legPreferences(),
		Inputs: s.Runtime.RoutingInputs(), Now: s.now(),
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
	// Only a route with selectors pays to describe the request to them.
	if x.route != nil && len(x.route.Selectors) > 0 {
		options.Features = x.features()
		options.Evaluate = s.evaluator(ctx, x, options.Features)
		if x.authorize != nil {
			options.Permitted = func(route runtime.Route) bool { return x.authorize(&route) == nil }
		}
	}
	return options
}

// features describe the caller's request to route selectors, from what
// admission already computed: its estimated size, whether it streams, and the
// tools, modalities, structured output and reasoning effort it asks for.
// Attribution labels never take part.
func (x *execution) features() *runtime.Features {
	if x.parsed == nil {
		return runtime.DescribeRequest(x.operationName(), nil, nil)
	}
	return runtime.DescribeRequest(x.operationName(), x.parsed, x.sourceDemand(""))
}
