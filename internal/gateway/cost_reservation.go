package gateway

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// costReservation is what a request asks to hold against the cost budgets of its
// applicable caller and aggregate boundaries while it is in flight. The zero value holds nothing:
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
	return authority.ProjectAttributionBudgets.Matches(authority.Attribution) || policy.RouteLimits.CostBudgeted() || authority.OrganizationBudget.Limited() || authority.InstallationBudget.Limited() || authority.ProjectBudget.Limited() || policy.DailyCostLimit != nil || policy.MonthlyCostLimit != nil || policy.WeeklyCostLimit != nil ||
		policy.EndUserPolicy.Limits(authority.EndUserDigest).CostBudgeted() || authority.ProjectEndUserPolicy.Limits(authority.EndUserDigest).CostBudgeted() ||
		authority.BudgetGroupID != nil &&
			(authority.BudgetGroupDailyCostLimit != nil || authority.BudgetGroupMonthlyCostLimit != nil || authority.BudgetGroupWeeklyCostLimit != nil)
}

// attemptCostReservation holds the price of the one attempt a request runs on,
// such as a video create, which never fails over because a second target would
// mint a second job.
func (x *execution) attemptCostReservation(authority access.Authority, attempt runtime.Attempt) costReservation {
	if !costBudgeted(x.admissionAuthority(authority)) {
		return costReservation{}
	}
	bound := x.attemptCostBound(attempt)
	if bound.IsZero() {
		return costReservation{}
	}
	return costReservation{amount: bound.String(), requestID: x.request.accountingID()}
}

// costReservation prices the request for admission: the most it could cost
// across the attempts it may dispatch, from the price list the gateway holds,
// which is the revision accounting will pin it to. Settlement bills every
// attempt that reported usage, a retry through another credential slot
// included, so the bound is their sum; it is replaced by the cost of what was
// actually dispatched.
func (s *Server) costReservation(x *execution, authority access.Authority) costReservation {
	if !costBudgeted(x.admissionAuthority(authority)) {
		return costReservation{}
	}
	bound := s.routeCostBound(x)
	if bound.IsZero() {
		return costReservation{}
	}
	return costReservation{amount: bound.String(), requestID: x.request.accountingID()}
}

func (s *Server) routeCostBound(x *execution) usage.Cost {
	var bound usage.Cost
	attempts := x.attempts
	if retryDispatches(x.route) > 1 {
		// Retries are optional: an early cheap target can fail over without
		// using them, leaving the budget for a more expensive target's retries.
		// Price the dearest possible dispatches first to cover either path.
		attempts = slices.Clone(attempts)
		slices.SortStableFunc(attempts, func(a, b runtime.Attempt) int {
			return x.attemptCostBound(b).Cmp(x.attemptCostBound(a))
		})
	}
	s.walkDispatchable(x, attempts, func(attempt runtime.Attempt) {
		// Each visit is a dispatch that may be billed on its own.
		bound = bound.Add(x.attemptCostBound(attempt))
	})
	return bound
}

// reserveFallbackCost covers the remaining route's work alongside the bound
// already dispatched, even when an earlier attempt could not report its usage.
func (s *Server) reserveFallbackCost(ctx context.Context, x *execution) *Error {
	if x.lease == nil || !costBudgeted(x.admissionAuthority(x.authority)) {
		return nil
	}
	bound := x.spentCost.Add(s.routeCostBound(x))
	if bound.IsZero() {
		return nil
	}
	deadline, _ := ctx.Deadline()
	return s.growCost(ctx, x.lease, bound.String(), x.request.accountingID(), max(time.Until(deadline), time.Second))
}

// reserveSessionCost holds the price of a realtime session's selected attempt.
// A session admits apart from choosing its target, so the hold follows the
// selection, before the provider is dialled.
func (s *Server) reserveSessionCost(ctx context.Context, x *execution, authority access.Authority, attempt runtime.Attempt, ttl time.Duration) *Error {
	hold := x.attemptCostReservation(authority, attempt)
	if x.lease == nil || hold.amount == "" {
		return nil
	}
	return s.growCost(ctx, x.lease, hold.amount, hold.requestID, ttl)
}

func (s *Server) growCost(ctx context.Context, lease *limits.Lease, amount, requestID string, ttl time.Duration) *Error {
	decision, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	err := lease.GrowCost(decision, amount, requestID, ttl)
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		s.Admission.recordRejection(exceeded.Dimension)
		return rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate)
	}
	if err != nil {
		return limitsUnavailable()
	}
	return nil
}

// attemptCostBound is the most one attempt could cost, and nothing when it has no
// price list to be priced by or the price list lacks a rate the request needs:
// an attempt nobody can price is not reserved for, and accounting records it as
// unpriced as it always has. Prices come from the routing inputs the gateway
// refreshes, and a stale list prices nothing.
func (x *execution) attemptCostBound(attempt runtime.Attempt) usage.Cost {
	if x.callerCostExempt() || attempt.Price == nil {
		return usage.Cost{}
	}
	if x.media != nil {
		if x.media.Op != media.OpVideoCreate {
			return usage.Cost{}
		}
		seconds := "4"
		if x.media.Seconds != nil {
			seconds = *x.media.Seconds
		}
		bound, _ := attempt.Price.Price.Cost(usage.AttemptUsage{Complete: true, MediaUnits: &seconds})
		return bound
	}
	if x.parsed == nil && x.unary == nil {
		// Pinned native requests use the resource input estimate and the
		// ordinary default reply allowance when no canonical request exists.
		input, reply := max(resourceEstimate, x.estimate), int64(0)
		if x.operationName() == "generation" || x.operationName() == "realtime" {
			reply = estimate.DefaultOutputTokens
		}
		bound, _ := attempt.Price.CostBound(input, reply, reply > 0)
		return bound
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
func (x *execution) settledCost() usage.Cost { return x.costOf("") }

// costOf is settledCost restricted to the attempts whose spend counts against
// a capped owner, or over every attempt when owner is empty.
func (x *execution) costOf(owner string) usage.Cost {
	var total usage.Cost
	operation := x.operationName()
	for index := range x.facts {
		fact := &x.facts[index]
		if fact.BudgetExempt || !fact.UsageObserved || fact.Price == nil || owner != "" && !slices.Contains(fact.Budgets, owner) {
			continue
		}
		if cost, ok := fact.Price.Cost(attemptUsage(fact, operation)); ok {
			total = total.Add(cost)
		}
	}
	return total
}

// responseCost is what the request cost as the response to a caller can state it,
// and false when it cannot: that is when accounting would record the request as
// unpriced, or as having cost nothing it could be charged for. Every attempt that
// was billed or may have been has to be priced for the answer to be the cost of the
// request, as it has to be for the request's spend to be one a budget can count. An
// attempt that reached a provider with no price list, or whose price lacks a rate
// for what it used, is unpriced, and so is a success that reported no usage. An
// attempt that may have been billed without reporting usage is priced, and adds
// nothing until its usage arrives, as it adds nothing in the accounting tables.
func (x *execution) responseCost() (usage.Cost, bool) {
	var total usage.Cost
	billable := false
	operation := x.operationName()
	for index := range x.facts {
		fact := &x.facts[index]
		if !fact.UsageObserved && !fact.BillingUncertain {
			continue
		}
		billable = true
		if fact.Price == nil || !fact.UsageObserved && fact.Class == classSuccess {
			return usage.Cost{}, false
		}
		if !fact.UsageObserved {
			continue
		}
		cost, ok := fact.Price.Cost(attemptUsage(fact, operation))
		if !ok {
			return usage.Cost{}, false
		}
		total = total.Add(cost)
	}
	return total, billable
}

// settleAdmission finishes the reservations a request holds once it has ended,
// whether it succeeded, failed or its client went away. A request that was handed
// to a provider replaces the cost it reserved with the cost it incurred; one that
// never was gives everything back. Either way this runs before the usage event
// is consumed or after, and the reservation script makes the order immaterial.
func (s *Server) settleAdmission(ctx context.Context, x *execution) {
	s.settleCaps(ctx, x)
	if x.lease == nil {
		// A request that reserved nothing has nothing to settle, and is not made to
		// total the tokens it would have settled.
		return
	}
	x.recordCost()
	settleKey(ctx, x.lease, x.dispatched, x.settledTokens(), s.log)
}

// recordCost gives a lease that holds a cost estimate what the request cost, for
// settlement to replace the estimate with.
func (x *execution) recordCost() {
	if x.dispatched && x.lease.HasCostReservation() {
		x.lease.SetActualCost(x.settledCost().String())
	}
}
