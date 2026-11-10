package admission

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/scripts"
)

const (
	cleanupAttempts = 3
	cleanupTimeout  = 250 * time.Millisecond
	cleanupBackoff  = 25 * time.Millisecond
	settleTimeout   = 100 * time.Millisecond
)

type Lease struct {
	limiter *Limiter
	rate    *rateHold
	cost    *costHold
	joined  *Lease
}

type rateHold struct {
	hold             scripts.RateHold
	tokens           int64
	countsTokens     bool
	countsRequest    bool
	holdsConcurrency bool
	reported         *rateAnswer
}

type rateAnswer struct {
	state limits.RateState
	at    time.Time
}

type costHold struct {
	budget      CostBudget
	reservation *costReservation
	actual      string
}

type costReservation struct {
	hold  scripts.CostHold
	grace time.Duration
}

func (le *Lease) Join(other *Lease) {
	if le == nil || other == nil || le == other {
		return
	}
	if le.joined == nil {
		le.joined = other
		return
	}
	le.joined.Join(other)
}

func (le *Lease) CostOwnerIDs() []string {
	if le == nil {
		return nil
	}
	if le.cost == nil {
		return le.joined.CostOwnerIDs()
	}
	return append([]string{le.cost.budget.Owner.ID}, le.joined.CostOwnerIDs()...)
}

func (le *Lease) GrowCost(ctx context.Context, cost, charge, requestID string, ttl time.Duration) error {
	if le == nil {
		return nil
	}
	if err := le.joined.GrowCost(ctx, cost, charge, requestID, ttl); err != nil {
		return err
	}
	if le.cost == nil {
		return nil
	}
	grown := le.cost.budget
	grown.Estimate = &Estimate{Amount: grown.Owner.Basis.Pick(cost, charge), RequestID: requestID}
	if err := grown.Validate(); err != nil {
		return err
	}
	reservation, err := le.limiter.reserveCost(ctx, grown, ttl, le.cost.reservation != nil)
	if err != nil {
		return err
	}
	le.cost.reservation = reservation
	return nil
}

func (le *Lease) RateState() limits.RateState {
	if le == nil || le.rate == nil || le.rate.reported == nil {
		return limits.RateState{}
	}
	return le.rate.reported.state
}

func (le *Lease) RateReset() time.Duration {
	state := le.RateState()
	if !state.Limited() {
		return 0
	}
	left := state.ResetAfter - time.Since(le.rate.reported.at)
	if left <= 0 {
		return 0
	}
	return (left + time.Millisecond - 1).Truncate(time.Millisecond)
}

func (le *Lease) HasCostReservation() bool {
	return le != nil && (le.cost != nil && le.cost.reservation != nil || le.joined.HasCostReservation())
}

func (le *Lease) SetActualCost(cost, charge string) {
	if le == nil {
		return
	}
	if le.cost != nil {
		le.cost.actual = le.cost.budget.Owner.Basis.Pick(cost, charge)
	}
	le.joined.SetActualCost(cost, charge)
}

func (le *Lease) held() *costReservation {
	if le.cost == nil {
		return nil
	}
	return le.cost.reservation
}

func (le *Lease) SettleCost(ctx context.Context) error {
	if le == nil {
		return nil
	}
	err := le.joined.SettleCost(ctx)
	held := le.held()
	if held == nil || le.cost.actual == "" {
		return err
	}
	if zeroAmount(le.cost.actual) {
		return errors.Join(err, le.limiter.release(ctx, held.hold))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	return errors.Join(err, le.limiter.scripts.SettleCost(ctx, held.hold, le.cost.actual, max(held.grace, time.Millisecond)))
}

func zeroAmount(amount string) bool {
	digits := strings.Replace(amount, ".", "", 1)
	return digits != "" && strings.Trim(digits, "0") == ""
}

func (l *Limiter) release(ctx context.Context, hold scripts.CostHold) error {
	return cleanup(ctx, func(ctx context.Context) error { return l.scripts.SettleCost(ctx, hold, "0", 0) })
}

func (l *Limiter) releaseOnce(ctx context.Context, hold scripts.CostHold) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_ = l.scripts.SettleCost(ctx, hold, "0", 0)
}

func (le *Lease) abandonCost(ctx context.Context, outage bool) {
	held := le.held()
	switch {
	case held == nil:
	case outage:
		le.limiter.releaseOnce(ctx, held.hold)
	default:
		_ = le.limiter.release(ctx, held.hold)
	}
}

func (le *Lease) Refund(ctx context.Context) error {
	if le == nil {
		return nil
	}
	err := le.joined.Refund(ctx)
	if held := le.held(); held != nil {
		err = errors.Join(err, le.limiter.release(ctx, held.hold))
	}
	rate := le.rate
	if rate == nil {
		return err
	}
	tokens := int64(0)
	if rate.countsTokens {
		tokens = rate.tokens
	}
	return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
		return le.limiter.scripts.RefundRate(ctx, rate.hold, rate.countsRequest, tokens)
	}))
}

func (le *Lease) Reconcile(ctx context.Context, actualTokens int64) error {
	if le == nil {
		return nil
	}
	err := le.joined.Reconcile(ctx, actualTokens)
	rate := le.rate
	if rate == nil || !rate.countsTokens {
		return err
	}
	if actualTokens < 0 || actualTokens > limits.MaxCounter {
		return errors.Join(err, invalid("actual tokens must be a non-negative Lua-safe integer"))
	}
	return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
		return le.limiter.scripts.AddTokens(ctx, rate.hold, max(int64(0), actualTokens-rate.tokens))
	}))
}

func (le *Lease) Release(ctx context.Context) error {
	if le == nil {
		return nil
	}
	err := le.joined.Release(ctx)
	rate := le.rate
	if rate == nil || !rate.holdsConcurrency {
		return err
	}
	return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
		return le.limiter.scripts.ReleaseConcurrency(ctx, rate.hold)
	}))
}

func cleanup(ctx context.Context, run func(context.Context) error) error {
	// Cleanup must outlive the caller: a cancelled request still has to give back what it held.
	detached := context.WithoutCancel(ctx)
	failures := make([]error, 0, cleanupAttempts)
	for attempt := range cleanupAttempts {
		if attempt > 0 {
			time.Sleep(cleanupBackoff << (attempt - 1))
		}
		attemptCtx, cancel := context.WithTimeout(detached, cleanupTimeout)
		err := run(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		failures = append(failures, err)
	}
	return failures[len(failures)-1]
}
