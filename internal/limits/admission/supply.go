package admission

import (
	"context"
	"errors"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/scripts"
)

type QuotaProbe struct {
	Lookup            string
	RequestsPerMinute *int64
	TokensPerMinute   *int64
	MaxConcurrency    *int64
}

type CapProbe struct {
	OwnerID string
	Daily   *string
	Monthly *string
}

type CapState int

const (
	CapUnknown CapState = iota
	CapAvailable
	CapExhausted
)

type Headroom struct {
	Fraction float64
	Known    bool
}

func (l *Limiter) Supply(ctx context.Context, quotas []QuotaProbe, caps []CapProbe) ([]Headroom, []CapState) {
	query := scripts.SupplyQuery{Lookups: make([]string, len(quotas)), Caps: make([]scripts.CostCaps, len(caps))}
	for index, quota := range quotas {
		query.Lookups[index] = quota.Lookup
	}
	for index, probe := range caps {
		query.Caps[index] = l.caps(CostBudget{
			Owner: limits.Owner{ID: probe.OwnerID, Kind: limits.OwnerSupply, Basis: limits.BasisCost},
			Daily: probe.Daily, Monthly: probe.Monthly,
		})
	}
	answer := l.scripts.Supply(ctx, query)
	headroom := make([]Headroom, len(quotas))
	for index, quota := range quotas {
		if answer.Usage[index].Err == nil {
			headroom[index] = Headroom{Fraction: quota.headroom(answer.Usage[index].Usage), Known: true}
		}
	}
	states := make([]CapState, len(caps))
	for index, probe := range caps {
		states[index] = capState(l.repaired(ctx, probe.OwnerID, answer.Caps[index], func() error {
			return l.scripts.CheckCost(ctx, query.Caps[index])
		}))
	}
	return headroom, states
}

func capState(err error) CapState {
	if err == nil {
		return CapAvailable
	}
	if _, exhausted := errors.AsType[*limits.ExceededError](err); exhausted {
		return CapExhausted
	}
	return CapUnknown
}

func (q QuotaProbe) headroom(usage scripts.Usage) float64 {
	fraction := 1.0
	for _, dimension := range [...]struct {
		limit *int64
		used  int64
	}{
		{q.RequestsPerMinute, usage.RequestsThisMinute},
		{q.TokensPerMinute, usage.TokensThisMinute},
		{q.MaxConcurrency, usage.ConcurrentRequests},
	} {
		if dimension.limit != nil && *dimension.limit > 0 {
			fraction = min(fraction, max(0, float64(*dimension.limit-dimension.used)/float64(*dimension.limit)))
		}
	}
	return fraction
}
