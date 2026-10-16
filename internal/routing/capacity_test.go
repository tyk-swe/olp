package routing

import ()

func decisionsByProvider(plan Plan, ids []string) map[string]Decision {
	out := map[string]Decision{}
	for _, d := range plan.Decisions {
		out[d.ProviderID] = d
	}
	return out
}

// A per-target demand stands in for the request-wide one.
