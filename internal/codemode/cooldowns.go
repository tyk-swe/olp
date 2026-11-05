package codemode

import (
	"math"
	"time"
)

type Cooldown struct {
	Class      string     `json:"class"`
	Model      *string    `json:"model"`
	RetryAt    time.Time  `json:"retry_at"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

func EarliestRecovery(now time.Time, times ...*time.Time) *time.Time {
	var earliest *time.Time
	for _, candidate := range times {
		if candidate == nil || !candidate.After(now) {
			continue
		}
		if earliest == nil || candidate.Before(*earliest) {
			value := *candidate
			earliest = &value
		}
	}
	return earliest
}

func AllowanceResets(allowance *Allowance) []*time.Time {
	if allowance == nil {
		return nil
	}
	resets := []*time.Time{allowance.ResetsAt}
	if allowance.TokenObservation != nil {
		resets = append(resets, allowance.TokenObservation.ResetsAt)
	}
	if allowance.RequestObservation != nil {
		resets = append(resets, allowance.RequestObservation.ResetsAt)
	}
	for _, window := range allowanceWindows(*allowance) {
		resets = append(resets, window.ResetsAt)
	}
	return resets
}

func (r *Refusal) RetryAfter(now time.Time) time.Duration {
	if r == nil || r.RetryAt == nil || !r.RetryAt.After(now) {
		return 0
	}
	return time.Duration(math.Ceil(r.RetryAt.Sub(now).Seconds())) * time.Second
}
