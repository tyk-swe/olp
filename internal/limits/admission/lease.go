package admission

import (
	"context"
	"errors"
	"fmt"
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
	inputTokens      int64
	outputTokens     int64
	countsTokens     bool
	countsInput      bool
	countsOutput     bool
	countsRequest    bool
	holdsConcurrency bool
	creditCombined   bool
	reported         *rateAnswer
}

func (h *rateHold) countsAnyTokens() bool {
	return h.countsTokens || h.countsInput || h.countsOutput
}

type TokenUsage struct {
	Input  int64
	Output int64
	Total  int64
	Credit bool
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
	hold scripts.CostHold
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

func (le *Lease) settleCost(ctx context.Context) error {
	if le == nil {
		return nil
	}
	err := le.joined.settleCost(ctx)
	held := le.held()
	if held == nil || le.cost.actual == "" {
		return err
	}
	if zeroAmount(le.cost.actual) {
		return errors.Join(err, le.limiter.release(ctx, held.hold))
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	return errors.Join(err, le.limiter.scripts.SettleCost(ctx, held.hold, le.cost.actual, costGrace))
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

func (le *Lease) refund(ctx context.Context) error {
	if le == nil {
		return nil
	}
	err := le.joined.refund(ctx)
	if held := le.held(); held != nil {
		err = errors.Join(err, le.limiter.release(ctx, held.hold))
	}
	rate := le.rate
	if rate == nil {
		return err
	}
	tokens, input, output := rate.refundAmounts()
	return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
		return le.limiter.scripts.RefundRate(ctx, rate.hold, rate.countsRequest, tokens, input, output)
	}))
}

func (h *rateHold) refundAmounts() (tokens, input, output int64) {
	if h.countsTokens {
		tokens = h.tokens
	}
	if h.countsInput {
		input = h.inputTokens
	}
	if h.countsOutput {
		output = h.outputTokens
	}
	return tokens, input, output
}

func (le *Lease) Reconcile(ctx context.Context, actualTokens int64) error {
	return le.reconcileUsage(ctx, TokenUsage{Total: actualTokens})
}

func tokenAdjustment(counts, credit bool, actual, reserved int64) int64 {
	if !counts {
		return 0
	}
	if credit {
		return actual - reserved
	}
	return max(int64(0), actual-reserved)
}

func (h *rateHold) adjustments(usage TokenUsage) (combined, input, output int64) {
	return tokenAdjustment(h.countsTokens, usage.Credit && h.creditCombined, usage.Total, h.tokens),
		tokenAdjustment(h.countsInput, usage.Credit, usage.Input, h.inputTokens),
		tokenAdjustment(h.countsOutput, usage.Credit, usage.Output, h.outputTokens)
}

func (le *Lease) reconcileUsage(ctx context.Context, usage TokenUsage) error {
	if le == nil {
		return nil
	}
	err := le.joined.reconcileUsage(ctx, usage)
	rate := le.rate
	if rate == nil || !rate.countsAnyTokens() {
		return err
	}
	for _, actual := range [...]int64{usage.Input, usage.Output, usage.Total} {
		if actual < 0 || actual > limits.MaxCounter {
			return errors.Join(err, invalid("actual tokens must be a non-negative Lua-safe integer"))
		}
	}
	combined, input, output := rate.adjustments(usage)
	return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
		return le.limiter.scripts.AdjustTokens(ctx, rate.hold, combined, input, output)
	}))
}

func (le *Lease) releaseConcurrency(ctx context.Context) error {
	if le == nil {
		return nil
	}
	err := le.joined.releaseConcurrency(ctx)
	rate := le.rate
	if rate == nil || !rate.holdsConcurrency {
		return err
	}
	return errors.Join(err, cleanup(ctx, func(ctx context.Context) error {
		return le.limiter.scripts.ReleaseConcurrency(ctx, rate.hold)
	}))
}

func (le *Lease) Refund(ctx context.Context) error {
	if le == nil {
		return nil
	}
	if err := le.refund(ctx); err != nil {
		return fmt.Errorf("refund limit reservation: %w", err)
	}
	return nil
}

// Settle keeps what a dispatched request consumed and gives back the rest.
// Any cost recorded with SetActualCost settles last in a single attempt,
// since a lost settle only leaves the reservation to lapse and be removed
// by the request's spend accounting records.
func (le *Lease) Settle(ctx context.Context, actualTokens *int64) error {
	if actualTokens == nil {
		return le.SettleUsage(ctx, nil)
	}
	return le.SettleUsage(ctx, &TokenUsage{Total: *actualTokens})
}

func (le *Lease) SettleUsage(ctx context.Context, usage *TokenUsage) error {
	if le == nil {
		return nil
	}
	var err error
	if usage != nil {
		if e := le.reconcileUsage(ctx, *usage); e != nil {
			err = errors.Join(err, fmt.Errorf("reconcile limit reservation: %w", e))
		}
	}
	if e := le.releaseConcurrency(ctx); e != nil {
		err = errors.Join(err, fmt.Errorf("release limit reservation: %w", e))
	}
	if e := le.settleCost(ctx); e != nil {
		err = errors.Join(err, fmt.Errorf("settle cost reservation: %w", e))
	}
	return err
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
