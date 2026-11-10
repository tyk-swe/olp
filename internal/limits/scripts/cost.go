package scripts

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
)

var ErrDurableMissing = fmt.Errorf("durable debit projection is missing: %w", limits.ErrUninitializedCost)

type CostCaps struct {
	Owner     limits.Owner
	Daily     string
	Weekly    string
	Monthly   string
	Increases string
	Durable   bool
}

type CostHold struct {
	Owner   limits.Owner
	LeaseID string
}

type Durable struct {
	Version int64
	Open    string
	Blocked bool
}

type costKeys struct{ day, month, pending, expiry, week, durable string }

func (r Runner) costKeys(owner limits.Owner) costKeys {
	prefix := r.Key("{" + limits.SimpleUUID(owner.ID) + "}:" + string(owner.Basis.Normalized()))
	return costKeys{
		day: prefix + ":day", month: prefix + ":month", pending: prefix + ":pending",
		expiry: prefix + ":expiry", week: prefix + ":week", durable: prefix + ":durable",
	}
}

func (k costKeys) windows(durable bool) []string {
	keys := []string{k.day, k.month, k.pending, k.expiry, k.week}
	if durable {
		return append(keys, k.durable)
	}
	return keys
}

func (r Runner) costCall(c CostCaps, amount, leaseID string, ttl time.Duration) call {
	return call{script: reserveCost, keys: r.costKeys(c.Owner).windows(c.Durable), args: []string{
		c.Daily, c.Monthly, "0", amount, leaseID, strconv.FormatInt(ttl.Milliseconds(), 10), c.Weekly, c.Increases,
	}}
}

func (r Runner) ReserveCost(ctx context.Context, c CostCaps, amount, leaseID string, ttl time.Duration) error {
	return r.decided(ctx, r.costCall(c, amount, leaseID, ttl), c.Owner.Basis)
}

func (r Runner) VerifyCost(ctx context.Context, c CostCaps) error {
	return r.decided(ctx, r.costCall(c, "verify", "", 0), c.Owner.Basis)
}

func (r Runner) CheckCost(ctx context.Context, c CostCaps) error {
	return r.decided(ctx, r.costCall(c, "check", "", 0), c.Owner.Basis)
}

func (r Runner) decided(ctx context.Context, c call, basis limits.Basis) error {
	value, err := r.run(ctx, c)
	if err != nil {
		return err
	}
	return decodeCost(value, basis)
}

func decodeCost(value any, basis limits.Basis) error {
	items, _ := value.([]any)
	h, ok := decodeHead(items, costResponseVersion)
	if !ok || len(items) != headFields || h.first < 1 || h.second < 1 {
		return limits.ErrUnexpectedResponse
	}
	failed := h.status == -1 && h.retry == 0
	switch {
	case h.status == 1 && h.detail == "ok" && h.retry == 0:
		return nil
	case h.status == 0 && h.retry >= 1:
		return costExceeded(h, basis)
	case failed && h.detail == "uninitialized_durable_cost_state":
		return ErrDurableMissing
	case failed && (h.detail == "unsettled_durable_cost_state" || h.detail == "uninitialized_daily_cost_state" ||
		h.detail == "uninitialized_monthly_cost_state" || h.detail == "uninitialized_weekly_cost_state"):
		return limits.ErrUninitializedCost
	case failed && malformedCost(h.detail):
		return limits.ErrMalformedState
	default:
		return limits.ErrUnexpectedResponse
	}
}

func malformedCost(detail string) bool {
	switch detail {
	case "malformed_durable_cost_state", "malformed_daily_cost_state", "malformed_monthly_cost_state", "malformed_weekly_cost_state":
		return true
	default:
		return false
	}
}

var costWindows = [...]struct {
	dimension limits.Dimension
	longest   int64
}{
	{limits.DimensionDailyCost, 3 * dayMS},
	{limits.DimensionWeeklyCost, 9 * dayMS},
	{limits.DimensionMonthlyCost, maxMonthMS},
}

func costExceeded(h head, basis limits.Basis) error {
	for _, window := range costWindows {
		name := string(window.dimension)
		if (h.detail == name || h.detail == name+"_estimate") && h.retry <= window.longest {
			return &limits.ExceededError{
				Dimension: window.dimension, Basis: basis, Estimate: h.detail != name,
				RetryAfter: time.Duration(h.retry) * time.Millisecond,
			}
		}
	}
	return limits.ErrUnexpectedResponse
}

func (r Runner) SettleCost(ctx context.Context, h CostHold, amount string, grace time.Duration) error {
	keys := r.costKeys(h.Owner)
	value, err := r.run(ctx, call{script: settleCost, keys: []string{keys.pending, keys.expiry},
		args: []string{h.LeaseID, amount, strconv.FormatInt(grace.Milliseconds(), 10)}})
	if err != nil {
		return err
	}
	f, ok := decodeFlags(value)
	if !ok || f.second != 0 || f.status != 1 || f.detail != "ok" || f.first != 0 && f.first != 1 {
		return limits.ErrUnexpectedResponse
	}
	return nil
}

func (r Runner) PendingCost(ctx context.Context, owner limits.Owner) (string, error) {
	value, err := r.Do(ctx, "HGET", r.costKeys(owner).pending, "sum")
	if err != nil {
		return "", err
	}
	switch total := value.(type) {
	case nil:
		return "0", nil
	case string:
		if limits.ValidDecimal(total) {
			return total, nil
		}
	}
	return "", limits.ErrMalformedState
}

func (r Runner) InstallSnapshot(ctx context.Context, s limits.Snapshot, durable *Durable) (limits.Installed, error) {
	if err := s.Validate(); err != nil {
		return limits.Installed{}, err
	}
	if durable != nil && (durable.Version < 0 || durable.Version > limits.MaxCounter || !limits.ValidDecimal(durable.Open) || s.Windows.DailyStart.IsZero()) {
		return limits.Installed{}, &limits.InvalidRequestError{Reason: "a durable projection needs a version, an open amount and calendar bounds"}
	}
	value, err := r.run(ctx, call{
		script: reconcileCost,
		keys:   r.costKeys(limits.Owner{ID: s.OwnerID, Basis: s.Basis}).windows(durable != nil),
		args:   snapshotArgs(s, durable),
	})
	if err != nil {
		return limits.Installed{}, err
	}
	f, ok := decodeFlags(value)
	switch {
	case ok && f.status == 1 && f.detail == "ok" && (f.first == 0 || f.first == 1) && (f.second == 0 || f.second == 1):
		return limits.Installed{Daily: f.first == 1, Monthly: f.second == 1}, nil
	case ok && f.status == -1 && f.first == 0 && f.second == 0 && malformedCost(f.detail):
		return limits.Installed{}, limits.ErrMalformedState
	default:
		return limits.Installed{}, limits.ErrUnexpectedResponse
	}
}

func snapshotArgs(s limits.Snapshot, durable *Durable) []string {
	requestID := ""
	if s.RequestID != "" {
		requestID = limits.CanonicalUUID(s.RequestID)
	}
	w := s.Windows
	accrued := []string{
		strconv.FormatInt(w.DailyID, 10), s.DailyAccrued, strconv.FormatInt(w.MonthlyID, 10), s.MonthlyAccrued,
		strconv.FormatInt(s.UnpricedAttempts, 10), "0", requestID, strconv.FormatInt(w.WeeklyID, 10), s.WeeklyAccrued,
	}
	if w.DailyStart.IsZero() {
		return accrued
	}
	bounded := accrued
	for _, instant := range [...]time.Time{w.DailyStart, w.DailyEnd, w.MonthlyStart, w.MonthlyEnd, w.WeeklyStart, w.WeeklyEnd} {
		bounded = append(bounded, strconv.FormatInt(instant.UnixMilli(), 10))
	}
	if durable == nil {
		return bounded
	}
	return append(bounded, strconv.FormatInt(durable.Version, 10), durable.Open, switchArg(durable.Blocked))
}

type SupplyQuery struct {
	Lookups []string
	Caps    []CostCaps
}

type SupplyAnswer struct {
	Usage []UsageAnswer
	Caps  []error
}

type UsageAnswer struct {
	Usage Usage
	Err   error
}

func (r Runner) Supply(ctx context.Context, q SupplyQuery) SupplyAnswer {
	calls := make([]call, 0, len(q.Lookups)+len(q.Caps))
	for _, lookup := range q.Lookups {
		calls = append(calls, r.usageCall(lookup))
	}
	for _, caps := range q.Caps {
		calls = append(calls, r.costCall(caps, "check", "", 0))
	}
	replies := r.runAll(ctx, calls)
	usage := make([]UsageAnswer, len(q.Lookups))
	for index := range q.Lookups {
		usage[index] = UsageAnswer{Err: replies[index].err}
		if replies[index].err == nil {
			usage[index].Usage, usage[index].Err = decodeUsage(replies[index].value)
		}
	}
	decisions := make([]error, len(q.Caps))
	for index, caps := range q.Caps {
		answer := replies[len(q.Lookups)+index]
		decisions[index] = answer.err
		if answer.err == nil {
			decisions[index] = decodeCost(answer.value, caps.Owner.Basis)
		}
	}
	return SupplyAnswer{Usage: usage, Caps: decisions}
}
