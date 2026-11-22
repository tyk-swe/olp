package codemode

import (
	"iter"
	"math"
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

type allowanceExhaustion struct {
	resetsAt   *time.Time
	observedAt time.Time
	count      bool
}

func allowanceExhaustions(allowance *Allowance, bandOrdered bool, family string) iter.Seq[allowanceExhaustion] {
	return func(yield func(allowanceExhaustion) bool) {
		if allowance == nil {
			return
		}
		for _, window := range allowance.Windows {
			if window.EffectiveEvidence() == AllowanceEvidencePassive && (!bandOrdered || family != "" && window.LimitID == family) &&
				(window.EffectiveUnit() == AllowanceUnitPercent && window.RemainingPercent == 0 ||
					slices.Contains([]AllowanceUnit{AllowanceUnitCount, AllowanceUnitCredits, AllowanceUnitCents}, window.EffectiveUnit()) && window.RemainingValue != nil && *window.RemainingValue == 0) &&
				!yield(allowanceExhaustion{resetsAt: window.ResetsAt, observedAt: window.ObservedAt}) {
				return
			}
		}
		if allowance.RemainingPercent != nil && *allowance.RemainingPercent == 0 && (!bandOrdered || family == "codex") &&
			!slices.ContainsFunc(allowance.Windows, func(window AllowanceWindow) bool {
				return window.LimitID == "codex" && window.Window == allowancePrimary
			}) && !yield(allowanceExhaustion{resetsAt: allowance.ResetsAt, observedAt: allowance.ObservedAt}) {
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
			if !yield(allowanceExhaustion{resetsAt: observation.ResetsAt, observedAt: observation.ObservedAt, count: true}) {
				return
			}
		}
	}
}

func ExhaustedResets(allowance *Allowance, now time.Time, bandOrdered bool, family string) []*time.Time {
	return slices.Collect(func(yield func(*time.Time) bool) {
		for exhaustion := range allowanceExhaustions(allowance, bandOrdered, family) {
			if !observationBlocks(exhaustion.resetsAt, exhaustion.observedAt, bandOrdered, now) {
				continue
			}
			if exhaustion.count && bandOrdered && exhaustion.resetsAt == nil {
				expiresAt := exhaustion.observedAt.Add(bandFreshness)
				if !yield(&expiresAt) {
					return
				}
				continue
			}
			if !yield(exhaustion.resetsAt) {
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

func (r *Refusal) RetryAfter(now time.Time) time.Duration {
	if r == nil || r.RetryAt == nil || !r.RetryAt.After(now) {
		return 0
	}
	return time.Duration(math.Ceil(r.RetryAt.Sub(now).Seconds())) * time.Second
}
