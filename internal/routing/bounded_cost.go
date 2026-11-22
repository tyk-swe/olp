package routing

type costTier struct {
	unhealthy, shadow                     bool
	priority, locality, order, preference int
}

// applyBoundedCost decides which eligible candidates the bounded_cost
// strategy orders by upper estimate. Within each health, priority, locality
// and preference tier, candidates with a known bound rank first only when at
// least two of them have one; otherwise the whole tier keeps weighted order.
// Every candidate left in weighted order names why.
func applyBoundedCost(rows []rankedCandidate, strategy, operation string) {
	if strategy != strategyBoundedCost {
		return
	}
	tiers := map[costTier][]int{}
	for i, row := range rows {
		if row.decision.Eligible {
			key := costTier{row.decision.Unhealthy, row.shadow, row.attempt.Priority, row.locality, row.order, row.preference}
			tiers[key] = append(tiers[key], i)
		}
	}
	for _, tier := range tiers {
		rankTier(rows, tier, tierFallback(rows, tier, operation))
	}
}

func tierFallback(rows []rankedCandidate, tier []int, operation string) string {
	if operation != "generation" {
		return "operation_not_generation"
	}
	bounded := 0
	for _, i := range tier {
		if rows[i].bound != nil {
			bounded++
		}
	}
	if bounded < 2 {
		return "insufficient_cost_bounds"
	}
	return ""
}

func rankTier(rows []rankedCandidate, tier []int, fallback string) {
	if len(tier) < 2 {
		return
	}
	for _, i := range tier {
		switch {
		case fallback != "":
			rows[i].decision.BoundedCostFallbackReason = strptr(fallback)
		case rows[i].bound == nil:
			rows[i].decision.BoundedCostFallbackReason = strptr("cost_bound_unknown")
		default:
			rows[i].costRanked = true
		}
	}
}

func compareBoundedCost(a, b rankedCandidate) int {
	if a.costRanked != b.costRanked {
		if a.costRanked {
			return -1
		}
		return 1
	}
	if !a.costRanked {
		return 0
	}
	return a.bound.Cost.Cmp(b.bound.Cost)
}
