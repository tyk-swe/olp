package admission

import (
	"context"
	"errors"
	"time"

	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/platform/coordination"
)

type DurableSyncer interface {
	SyncOwner(ctx context.Context, ownerID string) error
}

type Limiter struct {
	scripts scripts.Runner
	durable DurableSyncer
}

func NewDurable(runner scripts.Runner, durable DurableSyncer) *Limiter {
	return &Limiter{scripts: runner, durable: durable}
}

func (l *Limiter) Ping(ctx context.Context) error { return l.scripts.Ping(ctx) }

func (l *Limiter) Reserve(ctx context.Context, r Request) (*Lease, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	// Cost is judged before rate because a rejected rate reservation can give the cost hold back, and not the reverse.
	cost, err := l.holdCost(ctx, r.Cost, r.LeaseTTL)
	if err != nil {
		return nil, err
	}
	lease := &Lease{limiter: l, cost: cost}
	if !r.Rate.applies() {
		return lease, nil
	}
	rate, err := l.reserveRate(ctx, r)
	if err != nil {
		_, outage := errors.AsType[*limits.ServiceError](err)
		lease.abandonCost(ctx, outage)
		return nil, err
	}
	return &Lease{limiter: l, cost: cost, rate: rate}, nil
}

func (l *Limiter) holdCost(ctx context.Context, budget *limits.CostBudget, ttl time.Duration) (*costHold, error) {
	if budget == nil {
		return nil, nil
	}
	reservation, err := l.reserveCost(ctx, *budget, ttl, false)
	if err != nil {
		return nil, err
	}
	return &costHold{budget: *budget, reservation: reservation}, nil
}

func (l *Limiter) reserveRate(ctx context.Context, r Request) (*rateHold, error) {
	id := ids.New()
	reservation := scripts.RateReservation{
		Lookup: r.Lookup, LeaseID: id, TTL: r.LeaseTTL,
		Tokens: r.Rate.Tokens, InputTokens: r.Rate.InputTokens, OutputTokens: r.Rate.OutputTokens,
		RequestsPerMinute: ptr.Value(r.Rate.RequestsPerMinute), TokensPerMinute: ptr.Value(r.Rate.TokensPerMinute),
		InputTokensPerMinute: ptr.Value(r.Rate.InputTokensPerMinute), OutputTokensPerMinute: ptr.Value(r.Rate.OutputTokensPerMinute),
		MaxConcurrency: ptr.Value(r.Rate.MaxConcurrency), TrackConcurrency: r.Rate.TrackConcurrency,
		Stated: r.Rate.reports(),
	}
	if r.Rate.Share.divides() {
		reservation.LeaseID = r.Rate.Share.Class + "." + id
		reservation.Share = (*scripts.Share)(&r.Rate.Share)
	}
	if r.Membership != nil {
		reservation.Member = r.Membership.Member
		reservation.Generation = r.Membership.Generation
	}
	granted, err := l.scripts.ReserveRate(ctx, reservation)
	if err != nil {
		return nil, err
	}
	hold := &rateHold{
		hold: scripts.RateHold{
			Lookup: r.Lookup, LeaseID: reservation.LeaseID, WindowID: granted.WindowID, Class: r.Rate.Share.Class,
		},
		tokens:           r.Rate.Tokens,
		inputTokens:      r.Rate.InputTokens,
		outputTokens:     r.Rate.OutputTokens,
		countsTokens:     r.Rate.TokensPerMinute != nil,
		countsInput:      r.Rate.InputTokensPerMinute != nil,
		countsOutput:     r.Rate.OutputTokensPerMinute != nil,
		countsRequest:    r.Rate.RequestsPerMinute != nil,
		holdsConcurrency: granted.HoldsConcurrency,
		creditCombined:   r.Rate.CreditCombined,
	}
	if reservation.Stated {
		hold.reported = &rateAnswer{state: granted.Rate, at: time.Now()}
	}
	return hold, nil
}

func (l *Limiter) reserveCost(ctx context.Context, b limits.CostBudget, ttl time.Duration, retain bool) (*costReservation, error) {
	if b.Estimate == nil {
		return nil, l.decided(ctx, b.Owner.ID, func() error {
			return l.scripts.ReserveCost(ctx, scripts.BudgetCaps(b), "0", "", 0)
		})
	}
	held := &costReservation{hold: scripts.CostHold{Owner: b.Owner, LeaseID: limits.CanonicalUUID(b.Estimate.RequestID)}}
	err := l.decided(ctx, b.Owner.ID, func() error {
		return l.scripts.ReserveCost(ctx, scripts.BudgetCaps(b), b.Estimate.Amount, held.hold.LeaseID, ttl+costGrace)
	})
	if err == nil {
		return held, nil
	}
	// A failure that may have landed still holds the estimate, so it is given back.
	if !retain && ambiguous(err) {
		l.releaseOnce(ctx, held.hold)
	}
	return nil, err
}

func (l *Limiter) decided(ctx context.Context, ownerID string, decide func() error) error {
	return l.repaired(ctx, ownerID, decide(), decide)
}

func (l *Limiter) repaired(ctx context.Context, ownerID string, decision error, again func() error) error {
	if !errors.Is(decision, scripts.ErrDurableMissing) {
		return decision
	}
	if err := l.durable.SyncOwner(ctx, ownerID); err != nil {
		return err
	}
	return again()
}

func (l *Limiter) SyncSharedMemberships(ctx context.Context, generation int64, updates []limits.SharedMembership) error {
	return l.scripts.SyncMemberships(ctx, generation, updates)
}

func (l *Limiter) InstallSnapshot(ctx context.Context, s limits.Snapshot) (limits.Installed, error) {
	return l.scripts.InstallSnapshot(ctx, s, nil)
}

func (l *Limiter) InstallSnapshots(ctx context.Context, snapshots []limits.Snapshot) error {
	failures := make([]error, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if _, err := l.InstallSnapshot(ctx, snapshot); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func ambiguous(err error) bool {
	if _, ok := errors.AsType[*limits.ServiceError](err); !ok {
		return true
	}
	command, ok := errors.AsType[*coordination.CommandError](err)
	return !ok || command.Ambiguous
}
