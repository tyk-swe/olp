package code

import (
	"slices"
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

type CooldownScope string

const (
	ScopeCredential CooldownScope = "credential"
	ScopeModel      CooldownScope = "model"
)

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

func ExhaustedResets(allowance *Allowance, now time.Time, bandOrdered bool, family string) []*time.Time {
	if allowance == nil {
		return nil
	}
	return slices.Collect(func(yield func(*time.Time) bool) {
		for _, raw := range allowance.Windows {
			window := raw.Normalized()
			if window.Evidence == AllowanceEvidencePassive && (!bandOrdered || family != "" && window.LimitID == family) &&
				(window.Unit == AllowanceUnitPercent && window.RemainingPercent == 0 ||
					slices.Contains([]AllowanceUnit{AllowanceUnitCount, AllowanceUnitCredits, AllowanceUnitCents}, window.Unit) && window.RemainingValue != nil && *window.RemainingValue == 0) &&
				observationBlocks(window.ResetsAt, window.ObservedAt, bandOrdered, now) && !yield(window.ResetsAt) {
				return
			}
		}
		if allowance.RemainingPercent != nil && *allowance.RemainingPercent == 0 && (!bandOrdered || family == codexLimitID) &&
			observationBlocks(allowance.ResetsAt, allowance.ObservedAt, bandOrdered, now) && !slices.ContainsFunc(allowance.Windows, func(window AllowanceWindow) bool {
			return window.LimitID == codexLimitID && window.Window == allowancePrimary
		}) && !yield(allowance.ResetsAt) {
			return
		}
		for _, count := range []struct {
			remaining   *int64
			observation *CountObservation
		}{{allowance.RemainingTokens, allowance.TokenObservation}, {allowance.RemainingRequests, allowance.RequestObservation}} {
			if count.remaining == nil || *count.remaining != 0 {
				continue
			}
			observation := allowanceCountObservation(count.remaining, count.observation, *allowance)
			if !observationBlocks(observation.ResetsAt, observation.ObservedAt, bandOrdered, now) {
				continue
			}
			if bandOrdered && observation.ResetsAt == nil {
				expiresAt := observation.ObservedAt.Add(bandFreshness)
				if !yield(&expiresAt) {
					return
				}
				continue
			}
			if !yield(observation.ResetsAt) {
				return
			}
		}
	})
}

func observationBlocks(reset *time.Time, observedAt time.Time, bandOrdered bool, now time.Time) bool {
	if reset != nil {
		return reset.After(now)
	}
	return !bandOrdered || now.Sub(observedAt) <= bandFreshness
}

func BlockingDeadline(now time.Time, unavailableUntil, cooldownRetryAt *time.Time, allowance *Allowance, bandOrdered bool, family string) *time.Time {
	resets := ExhaustedResets(allowance, now, bandOrdered, family)
	if slices.Contains(resets, nil) {
		return nil
	}
	return LatestDeadline(now, slices.Concat([]*time.Time{unavailableUntil, cooldownRetryAt}, resets)...)
}
