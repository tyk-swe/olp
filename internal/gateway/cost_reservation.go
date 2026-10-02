package gateway

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// costReservation is what a request asks to hold against the cost budgets of its
// API key and budget group while it is in flight. The zero value holds nothing:
// the request is judged on the spend accrued so far, which is how a request that
// nobody can price is treated.
type costReservation struct {
	// amount is the estimate as canonical decimal text, empty for none.
	amount string
	// requestID is the identifier accounting will record the request's spend
	// under, which is how that spend later replaces the reservation.
	requestID string
}

// costBudgeted reports whether any cost budget applies to the key, so that a key
// without one never has a cost estimated for it.
func costBudgeted(authority access.Authority) bool {
	policy := authority.Policy
	return policy.DailyCostLimit != nil || policy.MonthlyCostLimit != nil ||
		authority.BudgetGroupID != nil &&
			(authority.BudgetGroupDailyCostLimit != nil || authority.BudgetGroupMonthlyCostLimit != nil)
}

// costReservation prices the request for admission: the most it could cost
// across the attempts it may dispatch, from the price list the gateway holds,
// which is the revision accounting will pin it to. It is the largest of the
// attempts and not their sum, so failing over does not multiply the reservation;
// settlement replaces it with the cost of what was actually dispatched.
func (s *Server) costReservation(x *execution, authority access.Authority) costReservation {
	if !costBudgeted(authority) {
		return costReservation{}
	}
	var bound usage.Cost
	var visited runtime.Attempt
	var seen bool
	s.walkDispatchable(x, func(attempt runtime.Attempt) {
		// An attempt is visited once for each of its credential slots in turn.
		if seen && attempt.TargetID == visited.TargetID && attempt.ProviderID == visited.ProviderID &&
			attempt.UpstreamModel == visited.UpstreamModel {
			return
		}
		visited, seen = attempt, true
		if cost := x.attemptCostBound(attempt); cost.Cmp(bound) > 0 {
			bound = cost
		}
	})
	if bound.IsZero() {
		return costReservation{}
	}
	return costReservation{amount: bound.String(), requestID: x.request.accountingID()}
}

// attemptCostBound is the most one attempt could cost, and nothing when it has no
// price list to be priced by or the price list lacks a rate the request needs:
// an attempt nobody can price is not reserved for, and accounting records it as
// unpriced as it always has. Prices come from the routing inputs the gateway
// refreshes, and a stale list prices nothing.
func (x *execution) attemptCostBound(attempt runtime.Attempt) usage.Cost {
	if attempt.Price == nil {
		return usage.Cost{}
	}
	provider, ok := x.snapshot().Providers[attempt.ProviderID]
	if !ok {
		return usage.Cost{}
	}
	admitted, err := x.attemptEstimate(&provider, attempt.UpstreamModel)
	if err != nil {
		return usage.Cost{}
	}
	// Only a request with a reply to bound is a generation, whose output is
	// priced; an embedding or a native operation is priced on its input alone.
	bound, ok := attempt.Price.CostBound(admitted.input, admitted.reply, admitted.reply > 0)
	if !ok {
		return usage.Cost{}
	}
	return bound
}

// settledCost is what the request cost, from the attempts that reported usage,
// each priced by the price list it was dispatched under exactly as accounting
// will price it. An attempt whose usage was never observed, is still to be
// reconciled or may have been billed without being reported has no cost here,
// as it has none in the accounting tables until its evidence arrives. It is a
// pure function of the attempts, so it can be read at any point after they have
// been recorded.
func (x *execution) settledCost() usage.Cost {
	var total usage.Cost
	operation := x.operationName()
	for index := range x.facts {
		fact := &x.facts[index]
		if !fact.UsageObserved || fact.Price == nil {
			continue
		}
		if cost, ok := fact.Price.Cost(attemptUsage(fact, operation)); ok {
			total = total.Add(cost)
		}
	}
	return total
}

// settleAdmission finishes the reservations a request holds once it has ended,
// whether it succeeded, failed or its client went away. A request that was handed
// to a provider replaces the cost it reserved with the cost it incurred; one that
// never was gives everything back. Either way this runs before the usage event
// is consumed or after, and the reservation script makes the order immaterial.
func (s *Server) settleAdmission(ctx context.Context, x *execution) {
	if x.dispatched && x.lease.HasCostReservation() {
		x.lease.SetActualCost(x.settledCost().String())
	}
	settleKey(ctx, x.lease, x.dispatched, x.settledTokens(), s.log)
}
