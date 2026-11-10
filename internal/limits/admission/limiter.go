package admission

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/scripts"
)

type DurableSyncer interface {
	SyncOwner(ctx context.Context, ownerID string) error
}

type Limiter struct {
	scripts scripts.Runner
	durable DurableSyncer
}

func New(runner scripts.Runner) *Limiter { return &Limiter{scripts: runner} }

func NewDurable(runner scripts.Runner, durable DurableSyncer) *Limiter {
	return &Limiter{scripts: runner, durable: durable}
}

func (l *Limiter) Ping(ctx context.Context) error { return l.scripts.Ping(ctx) }

func (l *Limiter) PendingCost(ctx context.Context, ownerID string) (string, error) {
	if _, err := uuid.Parse(ownerID); err != nil {
		return "", invalid("cost owner ID must be a UUID")
	}
	return l.scripts.PendingCost(ctx, limits.Owner{ID: ownerID, Basis: limits.BasisCost})
}

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

func (l *Limiter) holdCost(ctx context.Context, budget *CostBudget, ttl time.Duration) (*costHold, error) {
	if budget == nil {
		return nil, nil
	}
	reservation, err := l.reserveCost(ctx, *budget, ttl, false)
	if err != nil {
		return nil, err
	}
	return &costHold{budget: *budget, reservation: reservation}, nil
}

func limit(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func (l *Limiter) reserveRate(ctx context.Context, r Request) (*rateHold, error) {
	id := uuid.Must(uuid.NewV7()).String()
	reservation := scripts.RateReservation{
		Lookup: r.Lookup, LeaseID: id, TTL: r.LeaseTTL, Tokens: r.Rate.Tokens,
		RequestsPerMinute: limit(r.Rate.RequestsPerMinute), TokensPerMinute: limit(r.Rate.TokensPerMinute),
		MaxConcurrency: limit(r.Rate.MaxConcurrency), TrackConcurrency: r.Rate.TrackConcurrency,
		Stated: r.Rate.reports(),
	}
	if r.Rate.Share.divides() {
		reservation.LeaseID = r.Rate.Share.Class + "." + id
		reservation.Share = (*scripts.Share)(&r.Rate.Share)
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
		countsTokens:     r.Rate.TokensPerMinute != nil,
		countsRequest:    r.Rate.RequestsPerMinute != nil,
		holdsConcurrency: granted.HoldsConcurrency,
	}
	if reservation.Stated {
		hold.reported = &rateAnswer{state: granted.Rate, at: time.Now()}
	}
	return hold, nil
}

func (b CostBudget) Caps(durable bool) scripts.CostCaps {
	return scripts.CostCaps{
		Owner: b.Owner, Daily: text(b.Daily), Weekly: text(b.Weekly), Monthly: text(b.Monthly),
		Increases: b.Increases, Durable: durable,
	}
}

func (l *Limiter) caps(b CostBudget) scripts.CostCaps { return b.Caps(l.durable != nil) }

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (l *Limiter) reserveCost(ctx context.Context, b CostBudget, ttl time.Duration, retain bool) (*costReservation, error) {
	if b.Estimate == nil {
		return nil, l.decided(ctx, b.Owner.ID, func() error {
			return l.scripts.ReserveCost(ctx, l.caps(b), "0", "", 0)
		})
	}
	held := &costReservation{
		hold:  scripts.CostHold{Owner: b.Owner, LeaseID: limits.CanonicalUUID(b.Estimate.RequestID)},
		grace: b.Estimate.grace(),
	}
	err := l.decided(ctx, b.Owner.ID, func() error {
		return l.scripts.ReserveCost(ctx, l.caps(b), b.Estimate.Amount, held.hold.LeaseID, ttl+held.grace)
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
