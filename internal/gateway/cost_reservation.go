package gateway

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/usage"
)

// costReservation is what a request asks to hold against the cost budgets of its
// applicable caller and aggregate boundaries while it is in flight. The zero value holds nothing:
// the request is judged on the spend accrued so far, which is how a request that
// nobody can price is treated.
type costReservation struct {
	// amount is the cost estimate as canonical decimal text, empty for none;
	// charge is the estimate a charge-basis budget enforces, which equals the
	// cost estimate when the consumer holds no rate card.
	amount, charge string
	// requestID is the identifier accounting will record the request's spend
	// under, which is how that spend later replaces the reservation.
	requestID string
}

// amountFor is the estimate one owner's basis enforces.
func (r costReservation) amountFor(basis limits.Basis) string {
	return basis.Pick(r.amount, r.charge)
}

// costBudgeted reports whether this request spends against a caller budget.
// Estimation and growth use the same named-route boundary as admission, even
// after the request moves to a fallback route.
func (x *call) costBudgeted(authority access.Authority) bool {
	authority = x.admissionAuthority(authority)
	return callerCostBudgeted(&authority, x.limitRoute())
}

// attemptCostReservation holds the price of the one attempt a request runs on,
// such as a video create, which never fails over because a second target would
// mint a second job.
func (x *call) attemptCostReservation(authority access.Authority, attempt routing.Attempt) costReservation {
	if !x.costBudgeted(authority) {
		return costReservation{}
	}
	bound := x.attemptCostBound(attempt)
	charge := x.attemptChargeBound(attempt, bound)
	if bound.IsZero() && charge.IsZero() {
		return costReservation{}
	}
	return costReservation{amount: bound.String(), charge: charge.String(), requestID: x.request.accountingID()}
}

// costReservation prices the request for admission: the most it could cost
// across the attempts it may dispatch, from the price list the gateway holds,
// which is the revision accounting will pin it to. Settlement bills every
// attempt that reported usage, a retry through another credential slot
// included, so the bound is their sum; it is replaced by the cost of what was
// actually dispatched.
func (s *Server) costReservation(x *call, authority access.Authority) costReservation {
	if s.Runtime != nil {
		if release := s.Runtime.Release(); release != nil {
			x.currentCommitments = release.Snapshot
		}
	}
	if !x.costBudgeted(authority) {
		return costReservation{}
	}
	if x.routingInputs == nil && s.Runtime != nil {
		x.routingInputs = s.Runtime.RoutingInputs()
	}
	bound := s.routeCostBound(x)
	charge := s.routeChargeBound(x)
	if bound.IsZero() && charge.IsZero() {
		return costReservation{}
	}
	return costReservation{amount: bound.String(), charge: charge.String(), requestID: x.request.accountingID()}
}

// routeChargeBound is the reservation's charge-side total over the same
// dispatches: each attempt's pinned card prices the bound it could bill, and
// an unpinned request keeps the cost bound, as accounting will charge it the
// same amount it costs.
func (s *Server) routeChargeBound(x *call) usage.Cost {
	return s.routeBound(x, func(attempt routing.Attempt) usage.Cost {
		return x.attemptChargeBound(attempt, x.attemptCostBound(attempt))
	})
}

func (s *Server) routeCostBound(x *call) usage.Cost {
	return s.routeBound(x, x.attemptCostBound)
}

func (s *Server) routeBound(x *call, boundOf func(routing.Attempt) usage.Cost) usage.Cost {
	var bound usage.Cost
	attempts := x.attempts
	if retryDispatches(x.route) > 1 {
		// Retries are optional: an early cheap target can fail over without
		// using them, leaving the budget for a more expensive target's retries.
		// Price the dearest possible dispatches first to cover either path.
		attempts = slices.Clone(attempts)
		slices.SortStableFunc(attempts, func(a, b routing.Attempt) int {
			return boundOf(b).Cmp(boundOf(a))
		})
	}
	s.walkDispatchable(x, attempts, func(attempt routing.Attempt) {
		// Each visit is a dispatch that may be billed on its own.
		bound = bound.Add(boundOf(attempt))
	})
	return bound
}

// reserveFallbackCost covers the remaining route's work alongside the bound
// already dispatched, even when an earlier attempt could not report its usage.
func (s *Server) reserveFallbackCost(ctx context.Context, x *call) *Error {
	if s.Runtime != nil {
		if release := s.Runtime.Release(); release != nil {
			x.currentCommitments = release.Snapshot
		}
	}
	if x.lease == nil || !x.costBudgeted(x.authority) {
		return nil
	}
	bound := x.spentCost.Add(s.routeCostBound(x))
	charge := x.spentCharge.Add(s.routeChargeBound(x))
	if bound.IsZero() && charge.IsZero() {
		return nil
	}
	deadline, _ := ctx.Deadline()
	return s.growCost(ctx, x.lease, bound.String(), charge.String(), x.request.accountingID(), max(time.Until(deadline), time.Second))
}

// reserveSessionCost holds the price of a realtime session's selected attempt.
// A session admits apart from choosing its target, so the hold follows the
// selection, before the provider is dialled.
func (s *Server) reserveSessionCost(ctx context.Context, x *call, authority access.Authority, attempt routing.Attempt, ttl time.Duration) *Error {
	if s.Runtime != nil {
		if release := s.Runtime.Release(); release != nil {
			x.currentCommitments = release.Snapshot
		}
	}
	if x.routingInputs == nil && s.Runtime != nil {
		x.routingInputs = s.Runtime.RoutingInputs()
	}
	hold := x.attemptCostReservation(authority, attempt)
	if x.lease == nil || hold.amount == "" {
		return nil
	}
	return s.growCost(ctx, x.lease, hold.amount, hold.charge, hold.requestID, ttl)
}

func (s *Server) growCost(ctx context.Context, lease *limits.Lease, cost, charge, requestID string, ttl time.Duration) *Error {
	if e := s.Admission.checkCostAccounting(ctx); e != nil {
		return e
	}
	if s.Admission != nil && s.Admission.PendingCostAccountingReady != nil {
		for _, owner := range lease.CostOwnerIDs() {
			if e := s.Admission.checkPendingCostAccounting(ctx, owner); e != nil {
				return e
			}
		}
	}
	decision, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	err := lease.GrowCost(decision, cost, charge, requestID, ttl)
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		s.Admission.recordRejection(exceeded.Dimension)
		return rateLimitedBasis(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate, exceeded.Basis)
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
func (x *call) attemptCostBound(attempt routing.Attempt) usage.Cost {
	if x.callerCostExempt() || attempt.Price == nil {
		return usage.Cost{}
	}
	if x.committed(attempt.ProviderID) {
		// Provisioned capacity owes nothing per attempt, so nothing is
		// reserved for it.
		return usage.Cost{}
	}
	var provider routing.Provider
	if snapshot := x.snapshot(); snapshot != nil {
		var ok bool
		if provider, ok = snapshot.Providers[attempt.ProviderID]; !ok && (x.generation.parsed != nil || x.unary != nil) {
			return usage.Cost{}
		}
	} else if x.generation.parsed != nil || x.unary != nil {
		return usage.Cost{}
	}
	bound, _ := x.targetCostBound(provider, routing.Target{ID: attempt.TargetID, ProviderID: attempt.ProviderID, ProviderModel: attempt.UpstreamModel}, attempt.Price, attempt.Discount)
	return bound
}

// attemptChargeBound is the most one attempt could charge under the card the
// request pinned — the explicit rate's own bound, or the cost bound marked up —
// and the cost bound itself when the request holds no card. A committed
// attempt bounds nothing on cost but is still billable through an explicit
// card rate, so the charge bound is computed independently.
func (x *call) attemptChargeBound(attempt routing.Attempt, costBound usage.Cost) usage.Cost {
	if x.callerCostExempt() {
		return usage.Cost{}
	}
	rev := x.attemptCardRevision(attempt.ProviderID, x.request.startedAt)
	if rev == nil {
		return costBound
	}
	provider, ok := x.snapshot().Providers[attempt.ProviderID]
	if !ok {
		return costBound
	}
	if x.media != nil || x.operationName() == "search" {
		u := usage.AttemptUsage{Complete: true}
		if x.media != nil {
			var ok bool
			u, ok = videoCreateUsage(x.media)
			if !ok {
				return costBound
			}
		}
		var priced *usage.Cost
		if attempt.Price != nil {
			if _, ok := attempt.Price.Cost(u, x.request.startedAt, true, attempt.Discount); ok {
				c := costBound
				priced = &c
			}
		}
		bound, _ := rev.Charge(u, x.request.startedAt, true, priced,
			provider.Kind, provider.VendorID, attempt.UpstreamModel, x.operationName())
		return bound
	}
	var input, reply int64
	generation := false
	if x.generation.parsed == nil && x.unary == nil {
		input = max(resourceEstimate, x.estimate)
		if x.operationName() == "generation" || x.operationName() == "realtime" {
			reply = estimate.DefaultOutputTokens
			generation = true
		}
	} else {
		admitted, err := x.attemptEstimate(&provider, attempt.TargetID, attempt.UpstreamModel)
		if err != nil {
			return usage.Cost{}
		}
		input, reply = admitted.input, admitted.reply
		generation = reply > 0
	}
	bound, _ := rev.ChargeBound(input, reply, generation, x.request.startedAt, costBound,
		provider.Kind, provider.VendorID, attempt.UpstreamModel, x.operationName())
	return bound
}

// targetCostBound is attemptCostBound for a planned candidate rather than a
// dispatched attempt: the same worst case under the same pinned price and
// discount, or false when the request cannot be bounded.
func (x *call) targetCostBound(provider routing.Provider, target routing.Target, price *usage.RoutingPrice, discount *usage.RoutingDiscount) (usage.Cost, bool) {
	if operationregistry.UnboundedWork(x.operationName()) {
		return usage.Cost{}, false
	}
	if x.callerCostExempt() || price == nil {
		return usage.Cost{}, false
	}
	if x.committed(provider.ID) {
		return usage.Cost{}, true
	}
	if x.media != nil {
		u, ok := videoCreateUsage(x.media)
		if !ok {
			return usage.Cost{}, false
		}
		return price.Cost(u, x.request.startedAt, true, discount)
	}
	if x.operationName() == "search" {
		return price.Cost(usage.AttemptUsage{Complete: true}, x.request.startedAt, true, discount)
	}
	if x.generation.parsed == nil && x.unary == nil {
		input, reply := max(resourceEstimate, x.estimate), int64(0)
		if x.operationName() == "generation" || x.operationName() == "realtime" {
			reply = estimate.DefaultOutputTokens
		}
		return price.CostBound(input, reply, reply > 0, x.request.startedAt, discount)
	}
	admitted, err := x.attemptEstimate(&provider, target.ID, target.ProviderModel)
	if err != nil {
		return usage.Cost{}, false
	}
	return price.CostBound(admitted.input, admitted.reply, admitted.reply > 0, x.request.startedAt, discount)
}

func videoCreateUsage(m *media.Request) (usage.AttemptUsage, bool) {
	if m == nil || m.Op != media.OpVideoCreate {
		return usage.AttemptUsage{}, false
	}
	seconds := "4"
	if m.Seconds != nil {
		seconds = *m.Seconds
	}
	return usage.AttemptUsage{Complete: true, MediaUnits: &seconds}, true
}

// committed reports whether the attempt's provider settled under provisioned
// capacity, whose usage has a price but no marginal cost.
func (x *call) committed(providerID string) bool {
	snapshot := x.snapshot()
	if snapshot == nil {
		return false
	}
	if x.currentCommitments != nil {
		snapshot = x.currentCommitments
	}
	return snapshot.CommitmentAt(providerID, time.Now()) != nil
}

// settledCost is what the request cost, from the attempts that reported usage,
// each priced by the price list it was dispatched under exactly as accounting
// will price it. An attempt whose usage was never observed, is still to be
// reconciled or may have been billed without being reported has no cost here,
// as it has none in the accounting tables until its evidence arrives. It is a
// pure function of the attempts, so it can be read at any point after they have
// been recorded.
func (x *call) settledCost() usage.Cost { return x.costOf("") }

// costOf is settledCost restricted to the attempts whose spend counts against
// a capped owner, or over every attempt when owner is empty.
func (x *call) costOf(owner string) usage.Cost {
	var total usage.Cost
	operation := x.operationName()
	for index := range x.facts {
		fact := &x.facts[index]
		if fact.BudgetExempt || !fact.UsageObserved || fact.Price == nil || owner != "" && !slices.Contains(fact.Budgets, owner) {
			continue
		}
		if cost, ok := fact.Price.Cost(attemptUsage(fact, operation), fact.StartedAt, attemptSuccessful(fact), fact.Discount); ok {
			if !x.factCommitted(fact) {
				total = total.Add(cost)
			}
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
func (x *call) responseCost() (usage.Cost, bool) {
	if x.cacheHit() {
		return usage.Cost{}, true
	}
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
		cost, ok := fact.Price.Cost(attemptUsage(fact, operation), fact.StartedAt, attemptSuccessful(fact), fact.Discount)
		if !ok {
			return usage.Cost{}, false
		}
		if !x.factCommitted(fact) {
			total = total.Add(cost)
		}
	}
	return total, billable
}

// settleAdmission finishes the reservations a request holds once it has ended,
// whether it succeeded, failed or its client went away. A request that was handed
// to a provider replaces the cost it reserved with the cost it incurred; one that
// never was gives everything back. Either way this runs before the usage event
// is consumed or after, and the reservation script makes the order immaterial.
func (s *Server) settleAdmission(ctx context.Context, x *call) {
	s.settleCaps(ctx, x)
	if x.cacheRateLease != nil {
		zero := int64(0)
		// A cache embedding uses this shared request reservation even when the
		// parent's subsequent token or cost admission fails.
		consumed := x.dispatched || x.cacheHit() || x.cache != nil && x.cache.embeddingDispatched
		settleKey(ctx, x.cacheRateLease, consumed, &zero, s.log)
	}
	if x.lease == nil {
		// A request that reserved nothing has nothing to settle, and is not made to
		// total the tokens it would have settled.
		return
	}
	s.recordCost(x)
	if x.operationOverride == "guardrail" {
		actual := x.estimate
		settleKey(ctx, x.lease, x.dispatched, &actual, s.log)
		return
	}
	settleKey(ctx, x.lease, x.dispatched || x.cacheHit(), x.settledTokens(), s.log)
}

// settledCharge is what the request's consumers owe from the attempts that
// reported usage, priced under the card each attempt resolved exactly as
// accounting will charge it: an unpriced or uncharged attempt adds nothing.
// Commitment zeroes a priced cost before the card sees it — a marked-up zero is
// zero, and a fact with no card charges its zero cost — while an explicit card
// rate still prices a committed or provider-unpriced attempt.
func (s *Server) settledCharge(x *call) usage.Cost {
	var total usage.Cost
	operation := x.operationName()
	for index := range x.facts {
		fact := &x.facts[index]
		if fact.BudgetExempt || !fact.UsageObserved {
			continue
		}
		var cost usage.Cost
		var costPtr *usage.Cost
		if fact.Price != nil {
			if priced, ok := fact.Price.Cost(attemptUsage(fact, operation), fact.StartedAt, attemptSuccessful(fact), fact.Discount); ok {
				cost = priced
				if x.factCommitted(fact) {
					cost = usage.Cost{}
				}
				costPtr = &cost
			}
		}
		rev := fact.cardRev
		if rev == nil {
			// No card: the charge is the cost itself.
			total = total.Add(cost)
			continue
		}
		provider := x.snapshot().Providers[fact.ProviderID]
		if charge, ok := rev.Charge(attemptUsage(fact, operation), fact.StartedAt, attemptSuccessful(fact), costPtr,
			provider.Kind, provider.VendorID, fact.UpstreamModel, operation); ok {
			total = total.Add(charge)
		}
	}
	return total
}

func (x *call) attemptCardRevision(providerID string, at time.Time) *usage.RoutingCardRevision {
	if x.cardPinned {
		return x.cardRev
	}
	inputs := x.routingInputs
	if inputs == nil {
		return nil
	}
	// The consumer is the aggregate-ownership project: the key's, then the
	// provider's, then the route's — the same precedence accounting applies.
	project := x.authority.ProjectID
	if project == nil {
		if snapshot := x.snapshot(); snapshot != nil {
			if provider, ok := snapshot.Providers[providerID]; ok {
				project = provider.ProjectID
			}
		}
	}
	if project == nil && x.route != nil {
		project = x.route.ProjectID
	}
	if revision, known := inputs.RateCardRevision(project, at); known && revision != nil {
		return revision
	}
	return nil
}

// recordCost gives a lease that holds a cost estimate what the request cost, for
// settlement to replace the estimate with.
func (s *Server) recordCost(x *call) {
	if x.cacheHit() && x.lease.HasCostReservation() {
		x.lease.SetActualCost(x.cache.budgetCost, x.cache.budgetCharge)
		return
	}
	if x.dispatched && x.lease.HasCostReservation() {
		x.lease.SetActualCost(x.settledCost().String(), s.settledCharge(x).String())
	}
}

func (x *call) factCommitted(fact *AttemptFact) bool {
	if fact.CommitmentPinned {
		return fact.Commitment != nil
	}
	snapshot := x.snapshot()
	if snapshot == nil {
		return false
	}
	p, ok := snapshot.Providers[fact.ProviderID]
	return ok && p.CommitmentAt(fact.StartedAt) != nil
}
