package routing

type costTier struct {
	unhealthy, shadow                     bool
	priority, locality, order, preference int
}

func (row rankedCandidate) costTier() costTier {
	return costTier{row.decision.Unhealthy, row.shadow, row.attempt.Priority, row.locality, row.order, row.preference}
}

func applyBoundedCost(rows []rankedCandidate, strategy, operation string, callerCostExempt bool) []rankedCandidate {
	if strategy != strategyBoundedCost {
		return rows
	}
	tiers := map[costTier][]rankedCandidate{}
	for _, row := range rows {
		if row.decision.Eligible {
			tiers[row.costTier()] = append(tiers[row.costTier()], row)
		}
	}
	ranked := make([]rankedCandidate, len(rows))
	for i, row := range rows {
		ranked[i] = boundedCostRow(row, tiers[row.costTier()], operation, callerCostExempt)
	}
	return ranked
}

func boundedCostRow(row rankedCandidate, tier []rankedCandidate, operation string, callerCostExempt bool) rankedCandidate {
	if !row.decision.Eligible || len(tier) < 2 {
		return row
	}
	ranked := row
	fallback := tierFallback(tier, operation, callerCostExempt)
	switch {
	case fallback != "":
		ranked.decision.BoundedCostFallbackReason = strptr(fallback)
	case row.bound == nil:
		ranked.decision.BoundedCostFallbackReason = strptr("cost_bound_unknown")
	default:
		ranked.costRanked = true
	}
	return ranked
}

func tierFallback(tier []rankedCandidate, operation string, callerCostExempt bool) string {
	if operation != "generation" {
		return "operation_not_generation"
	}
	if callerCostExempt || boundedCount(tier) < 2 {
		return "insufficient_cost_bounds"
	}
	return ""
}

func boundedCount(tier []rankedCandidate) int {
	count := 0
	for _, row := range tier {
		if row.bound != nil {
			count++
		}
	}
	return count
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
