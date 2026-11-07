package codemode

import (
	"math"
	"time"
)

type CooldownClass string

const (
	CooldownQuota       CooldownClass = "quota"
	CooldownEntitlement CooldownClass = "entitlement"
	CooldownRate        CooldownClass = "rate"
	CooldownCapacity    CooldownClass = "capacity"
	CooldownConcurrency CooldownClass = "concurrency"
)

func CooldownClasses() [5]CooldownClass {
	return [5]CooldownClass{CooldownQuota, CooldownEntitlement, CooldownRate, CooldownCapacity, CooldownConcurrency}
}

type Cooldown struct {
	Class      CooldownClass `json:"class"`
	Model      *string       `json:"model"`
	RetryAt    time.Time     `json:"retry_at"`
	ObservedAt *time.Time    `json:"observed_at,omitempty"`
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

func LatestDeadline(now time.Time, times ...*time.Time) *time.Time {
	var latest *time.Time
	for _, candidate := range times {
		if candidate == nil || !candidate.After(now) {
			continue
		}
		if latest == nil || candidate.After(*latest) {
			value := *candidate
			latest = &value
		}
	}
	return latest
}

func BlockingDeadline(now time.Time, unavailableUntil, cooldownRetryAt *time.Time, _ *Allowance, _ bool, _ string) *time.Time {
	return LatestDeadline(now, unavailableUntil, cooldownRetryAt)
}

func (r *Refusal) RetryAfter(now time.Time) time.Duration {
	if r == nil || r.RetryAt == nil || !r.RetryAt.After(now) {
		return 0
	}
	return time.Duration(math.Ceil(r.RetryAt.Sub(now).Seconds())) * time.Second
}
