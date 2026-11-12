package gateway

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/rates"

	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
)

// capHold is a request's reservation against one supply-side spend cap. The
// reservation grows before each dispatch to cover the estimates of all work
// handed to that owner. It settles once with the request.
type capHold struct {
	owner      string
	lease      *admission.Lease
	dispatched bool
	reserved   rates.Cost
	spent      rates.Cost
}

// capCheck is the outcome of holding an attempt's spend caps. Owners are the
// capped owners its spend counts against. Quota names the cap that stopped the
// attempt, if any: refusal is set when that cap is exhausted, and nil when its
// state could not be read.
type capCheck struct {
	owners  []string
	quota   string
	refusal *attemptFailure
}

// holdCaps reserves the attempt's estimated cost against every spend cap it
// is subject to. An exhausted cap refuses the attempt; a cap whose state
// cannot be read skips the target, as an unreadable quota does.
func (s *Server) holdCaps(ctx context.Context, x *call, attempt routing.Attempt, provider *routing.Provider, slot *routing.Slot, deadline time.Time) capCheck {
	var check capCheck
	if x.freeRetainedControl() {
		return check
	}
	if x.callerCostExempt() && provider.CredentialSource == "caller" {
		return check
	}
	checked := false
	for _, owner := range quota.AttemptCaps(x.route, provider, slot) {
		if owner.Limits == nil {
			continue
		}
		if !checked {
			if e := s.deps.Admission.CheckCostAccounting(ctx); e != nil {
				return capCheck{quota: owner.Quota}
			}
			checked = true
		}
		if e := s.deps.Admission.CheckPendingCostAccounting(ctx, owner.ID); e != nil {
			return capCheck{quota: owner.Quota}
		}
		check.owners = append(check.owners, owner.ID)
		index := slices.IndexFunc(x.caps, func(h capHold) bool { return h.owner == owner.ID })
		bound := x.attemptCostBound(attempt)
		if index >= 0 {
			bound = x.caps[index].spent.Add(bound)
			if bound.Cmp(x.caps[index].reserved) <= 0 {
				continue
			}
		}
		if !s.deps.Admission.Ready() {
			return capCheck{quota: owner.Quota}
		}
		ttl := max(time.Until(deadline), time.Second)
		decision, cancel := context.WithTimeout(ctx, quota.ReserveTimeout)
		lease, err := s.holdCap(decision, x, index, owner, bound, ttl)
		cancel()
		if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
			return capCheck{quota: owner.Quota, refusal: &attemptFailure{class: classBudget, quota: owner.Quota, retryAfter: exceeded.RetryAfter}}
		}
		if err != nil {
			s.deps.Admission.Logger().Warn("skipping a target whose spend cap is unreadable", "owner_id", owner.ID, "error", err.Error())
			return capCheck{quota: owner.Quota}
		}
		if index < 0 {
			x.caps = append(x.caps, capHold{owner: owner.ID, lease: lease, reserved: bound})
		} else {
			x.caps[index].lease, x.caps[index].reserved = lease, bound
		}
	}
	return check
}

func (s *Server) holdCap(ctx context.Context, x *call, index int, owner quota.CappedOwner, bound rates.Cost, ttl time.Duration) (*admission.Lease, error) {
	amount := quota.CostReservation{RequestID: x.request.accountingID()}
	if !bound.IsZero() {
		amount.Amount = bound.String()
	}
	if index < 0 || !x.caps[index].lease.HasCostReservation() {
		return s.deps.Admission.Reserve(ctx, amount.On(quota.CapRequest(owner, ttl)))
	}
	held := x.caps[index].lease
	return held, held.GrowCost(ctx, amount.Amount, amount.Amount, amount.RequestID, ttl)
}

// spendCaps marks the caps an attempt was dispatched against, whose
// reservations settle at cost rather than being given back.
func (x *call) spendCaps(owners []string, attempt routing.Attempt) {
	bound := x.attemptCostBound(attempt)
	x.spentCost = x.spentCost.Add(bound)
	x.spentCharge = x.spentCharge.Add(x.attemptChargeBound(attempt, bound))
	for i := range x.caps {
		if slices.Contains(owners, x.caps[i].owner) {
			x.caps[i].dispatched = true
			x.caps[i].spent = x.caps[i].spent.Add(bound)
		}
	}
}

// settleCaps replaces each cap reservation with what the attempts counted
// against its owner cost, or gives it back when none of them was dispatched.
func (s *Server) settleCaps(ctx context.Context, x *call) {
	for _, hold := range x.caps {
		if hold.dispatched && hold.lease.HasCostReservation() {
			actual := x.costOf(hold.owner).String()
			hold.lease.SetActualCost(actual, actual)
		}
		quota.SettleKey(ctx, hold.lease, hold.dispatched, nil, s.log)
	}
	x.caps = nil
}
