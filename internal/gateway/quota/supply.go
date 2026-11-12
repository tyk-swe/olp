package quota

import (
	"context"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
)

// ReadSupply reads the remaining headroom of the queried slots and the state of
// the queried spend caps in one round trip. A slot's headroom is the smaller
// of its own and its connection's; a slot with no quota has none to report,
// and ranks after slots whose headroom is known.
func (a *Admission) ReadSupply(ctx context.Context, query routing.SupplyQuery) *routing.SupplyState {
	var quotas []admission.QuotaProbe
	probe := func(lookup string, rpm, tpm, concurrency *int64) int {
		if rpm == nil && tpm == nil && concurrency == nil {
			return -1
		}
		if i := slices.IndexFunc(quotas, func(q admission.QuotaProbe) bool { return q.Lookup == lookup }); i >= 0 {
			return i
		}
		quotas = append(quotas, admission.QuotaProbe{Lookup: lookup, RequestsPerMinute: rpm, TokensPerMinute: tpm, MaxConcurrency: concurrency})
		return len(quotas) - 1
	}
	type slotProbes struct {
		ID               string
		connection, slot int
	}
	slots := make([]slotProbes, 0, len(query.Slots))
	for _, entry := range query.Slots {
		probes := slotProbes{ID: entry.Slot.ID, connection: -1}
		if quota := entry.Provider.Limits; quota != nil {
			probes.connection = probe(limits.ConnectionLookup(entry.Provider.ID), quota.RequestsPerMinute, quota.TokensPerMinute, quota.MaxConcurrency)
		}
		probes.slot = probe(limits.SlotLookup(entry.Slot.ID), entry.Slot.RequestsPerMinute, entry.Slot.TokensPerMinute, entry.Slot.MaxConcurrency)
		slots = append(slots, probes)
	}
	caps := make([]admission.CapProbe, len(query.Caps))
	for i, cap := range query.Caps {
		caps[i] = admission.CapProbe{OwnerID: cap.OwnerID, Daily: cap.Limits.DailyCostLimit, Monthly: cap.Limits.MonthlyCostLimit}
	}
	state := &routing.SupplyState{Headroom: map[string]float64{}, Spend: map[string]routing.Spend{}}
	if len(quotas) == 0 && len(caps) == 0 {
		return state
	}
	read, cancel := context.WithTimeout(ctx, ReserveTimeout)
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
			state.Headroom[slot.ID] = fraction
		}
	}
	for i, cap := range caps {
		switch spend[i] {
		case admission.CapAvailable:
			state.Spend[cap.OwnerID] = routing.SpendAvailable
		case admission.CapExhausted:
			state.Spend[cap.OwnerID] = routing.SpendExhausted
		}
	}
	return state
}

// cappedOwner is one spend cap an attempt is subject to.
type CappedOwner struct {
	ID, lookup, Quota string
	Limits            *routing.CostLimits
}

// attemptCaps lists the caps one attempt spends against: its route's, its
// connection's and its credential slot's, in that order.
func AttemptCaps(route *routing.Route, provider *routing.Provider, slot *routing.Slot) [3]CappedOwner {
	owners := [3]CappedOwner{
		{route.ID, limits.RouteLookup(route.ID), Route, route.Budget},
		{provider.ID, limits.ConnectionLookup(provider.ID), Connection, nil},
		{slot.ID, limits.SlotLookup(slot.ID), Slot, slot.Supply.Costs()},
	}
	if provider.Limits != nil {
		owners[1].Limits = provider.Limits.Costs()
	}
	return owners
}

func CapRequest(owner CappedOwner, ttl time.Duration) admission.Request {
	return admission.Request{
		Lookup: owner.lookup, LeaseTTL: ttl,
		Cost: admission.Budget(limits.Owner{ID: owner.ID, Kind: limits.OwnerSupply, Basis: limits.BasisCost}, owner.Limits.DailyCostLimit, nil, owner.Limits.MonthlyCostLimit, ""),
	}
}
