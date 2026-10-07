package gateway

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// ReadSupply reads the remaining headroom of the queried slots and the state of
// the queried spend caps in one round trip. A slot's headroom is the smaller
// of its own and its connection's; a slot with no quota has none to report,
// and ranks after slots whose headroom is known.
func (a *Admission) ReadSupply(ctx context.Context, query runtime.SupplyQuery) *runtime.SupplyState {
	var quotas []limits.QuotaProbe
	probe := func(lookup string, rpm, tpm, concurrency *int64) int {
		if rpm == nil && tpm == nil && concurrency == nil {
			return -1
		}
		if i := slices.IndexFunc(quotas, func(q limits.QuotaProbe) bool { return q.LookupID == lookup }); i >= 0 {
			return i
		}
		quotas = append(quotas, limits.QuotaProbe{LookupID: lookup, RequestsPerMinute: rpm, TokensPerMinute: tpm, MaxConcurrency: concurrency})
		return len(quotas) - 1
	}
	type slotProbes struct {
		id               string
		connection, slot int
	}
	slots := make([]slotProbes, 0, len(query.Slots))
	for _, entry := range query.Slots {
		probes := slotProbes{id: entry.Slot.ID, connection: -1}
		if quota := entry.Provider.Limits; quota != nil {
			probes.connection = probe(limits.ConnectionLookup(entry.Provider.ID), quota.RequestsPerMinute, quota.TokensPerMinute, quota.MaxConcurrency)
		}
		probes.slot = probe(limits.SlotLookup(entry.Slot.ID), entry.Slot.RequestsPerMinute, entry.Slot.TokensPerMinute, entry.Slot.MaxConcurrency)
		slots = append(slots, probes)
	}
	caps := make([]limits.CapProbe, len(query.Caps))
	for i, cap := range query.Caps {
		caps[i] = limits.CapProbe{OwnerID: cap.OwnerID, DailyCostLimit: cap.Limits.DailyCostLimit, MonthlyCostLimit: cap.Limits.MonthlyCostLimit}
	}
	state := &runtime.SupplyState{Headroom: map[string]float64{}, Spend: map[string]runtime.Spend{}}
	if len(quotas) == 0 && len(caps) == 0 {
		return state
	}
	read, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	headroom, spend := a.limiter.Supply(read, quotas, caps)

	for _, slot := range slots {
		fraction, known := 1.0, false
		for _, i := range [...]int{slot.connection, slot.slot} {
			if i < 0 {
				continue
			}
			if !headroom[i].Known {
				known = false
				break
			}
			fraction, known = min(fraction, headroom[i].Fraction), true
		}
		if known {
			state.Headroom[slot.id] = fraction
		}
	}
	for i, cap := range caps {
		switch spend[i] {
		case limits.CapAvailable:
			state.Spend[cap.OwnerID] = runtime.SpendAvailable
		case limits.CapExhausted:
			state.Spend[cap.OwnerID] = runtime.SpendExhausted
		}
	}
	return state
}

// capHold is a request's reservation against one supply-side spend cap. The
// reservation grows before each dispatch to cover the estimates of all work
// handed to that owner. It settles once with the request.
type capHold struct {
	owner      string
	lease      *limits.Lease
	dispatched bool
	reserved   usage.Cost
	spent      usage.Cost
}

// cappedOwner is one spend cap an attempt is subject to.
type cappedOwner struct {
	id, lookup, quota string
	limits            *runtime.CostLimits
}

// attemptCaps lists the caps one attempt spends against: its route's, its
// connection's and its credential slot's, in that order.
func attemptCaps(route *runtime.Route, provider *runtime.Provider, slot *runtime.Slot) [3]cappedOwner {
	owners := [3]cappedOwner{
		{route.ID, limits.RouteLookup(route.ID), quotaRoute, route.Budget},
		{provider.ID, limits.ConnectionLookup(provider.ID), quotaConnection, nil},
		{slot.ID, limits.SlotLookup(slot.ID), quotaSlot, slot.Supply.Costs()},
	}
	if provider.Limits != nil {
		owners[1].limits = provider.Limits.Costs()
	}
	return owners
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
func (s *Server) holdCaps(ctx context.Context, x *execution, attempt runtime.Attempt, provider *runtime.Provider, slot *runtime.Slot, deadline time.Time) capCheck {
	var check capCheck
	for _, owner := range attemptCaps(x.route, provider, slot) {
		if owner.limits == nil {
			continue
		}
		check.owners = append(check.owners, owner.id)
		index := slices.IndexFunc(x.caps, func(h capHold) bool { return h.owner == owner.id })
		bound := x.attemptCostBound(attempt)
		if index >= 0 {
			bound = x.caps[index].spent.Add(bound)
			if bound.Cmp(x.caps[index].reserved) <= 0 {
				continue
			}
		}
		if !s.Admission.ready() {
			return capCheck{quota: owner.quota}
		}
		request := limits.Request{
			LookupID: owner.lookup, CostOwnerID: owner.id, DailyCostLimit: owner.limits.DailyCostLimit,
			MonthlyCostLimit: owner.limits.MonthlyCostLimit, LeaseTTL: max(time.Until(deadline), time.Second),
			RetainCostReservation: index >= 0 && x.caps[index].lease.HasCostReservation(),
		}
		if !bound.IsZero() {
			request.CostEstimate, request.RequestID = bound.String(), x.request.accountingID()
		}
		decision, cancel := context.WithTimeout(ctx, reserveTimeout)
		lease, err := s.Admission.limiter.Reserve(decision, request)
		cancel()
		if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
			return capCheck{quota: owner.quota, refusal: &attemptFailure{class: classBudget, quota: owner.quota, retryAfter: exceeded.RetryAfter}}
		}
		if err != nil {
			s.Admission.logger().Warn("skipping a target whose spend cap is unreadable", "owner_id", owner.id, "error", err.Error())
			return capCheck{quota: owner.quota}
		}
		if index < 0 {
			x.caps = append(x.caps, capHold{owner: owner.id, lease: lease, reserved: bound})
		} else {
			x.caps[index].lease, x.caps[index].reserved = lease, bound
		}
	}
	return check
}

// spendCaps marks the caps an attempt was dispatched against, whose
// reservations settle at cost rather than being given back.
func (x *execution) spendCaps(owners []string, attempt runtime.Attempt) {
	bound := x.attemptCostBound(attempt)
	x.spentCost = x.spentCost.Add(bound)
	for i := range x.caps {
		if slices.Contains(owners, x.caps[i].owner) {
			x.caps[i].dispatched = true
			x.caps[i].spent = x.caps[i].spent.Add(bound)
		}
	}
}

// settleCaps replaces each cap reservation with what the attempts counted
// against its owner cost, or gives it back when none of them was dispatched.
func (s *Server) settleCaps(ctx context.Context, x *execution) {
	for _, hold := range x.caps {
		if hold.dispatched && hold.lease.HasCostReservation() {
			hold.lease.SetActualCost(x.costOf(hold.owner).String())
		}
		settleKey(ctx, hold.lease, hold.dispatched, nil, s.log)
	}
	x.caps = nil
}
