package codemode

import (
	"slices"
	"time"
)

type ThreadAvailability string

const (
	ThreadAvailable   ThreadAvailability = "available"
	ThreadUnavailable ThreadAvailability = "unavailable"
	ThreadUnknown     ThreadAvailability = "unknown"
)

type ThreadStatus struct {
	Status         ThreadAvailability `json:"status"`
	CheckedAt      time.Time          `json:"checked_at"`
	LastObservedAt *time.Time         `json:"last_observed_at"`
	RetryAt        *time.Time         `json:"retry_at"`
}

type ThreadAccountEvidence struct {
	Eligible           bool
	Health             string
	HealthObservedAt   *time.Time
	UnavailableUntil   *time.Time
	Allowance          *Allowance
	CooldownRetryAt    *time.Time
	CooldownObservedAt *time.Time
}

type exhaustedAllowanceEvidence struct {
	reset    *time.Time
	observed time.Time
}

func exhaustedAllowanceEvidences(allowance *Allowance) []exhaustedAllowanceEvidence {
	if allowance == nil {
		return nil
	}
	return slices.Collect(func(yield func(exhaustedAllowanceEvidence) bool) {
		for _, window := range allowance.Windows {
			if window.EffectiveEvidence() == AllowanceEvidencePassive &&
				(window.EffectiveUnit() == AllowanceUnitPercent && window.RemainingPercent == 0 ||
					slices.Contains([]AllowanceUnit{AllowanceUnitCount, AllowanceUnitCredits, AllowanceUnitCents}, window.EffectiveUnit()) && window.RemainingValue != nil && *window.RemainingValue == 0) &&
				!yield(exhaustedAllowanceEvidence{reset: window.ResetsAt, observed: window.ObservedAt}) {
				return
			}
		}
		if allowance.RemainingPercent != nil && *allowance.RemainingPercent == 0 &&
			!slices.ContainsFunc(allowance.Windows, func(window AllowanceWindow) bool {
				return window.LimitID == "codex" && window.Window == allowancePrimary
			}) && !yield(exhaustedAllowanceEvidence{reset: allowance.ResetsAt, observed: allowance.ObservedAt}) {
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
			if !yield(exhaustedAllowanceEvidence{reset: observation.ResetsAt, observed: observation.ObservedAt}) {
				return
			}
		}
	})
}

func latestTime(times ...*time.Time) *time.Time {
	var latest *time.Time
	for _, candidate := range times {
		if candidate == nil || candidate.IsZero() {
			continue
		}
		if latest == nil || candidate.After(*latest) {
			value := *candidate
			latest = &value
		}
	}
	return latest
}

type threadAccountAssessment struct {
	blocked      bool
	deadline     *time.Time
	positive     bool
	lastObserved *time.Time
}

func assessThreadAccount(now time.Time, evidence ThreadAccountEvidence) threadAccountAssessment {
	healthBlocking := evidence.Health == "unavailable" || evidence.Health == "quota_limited"
	healthBlocked := healthBlocking && !(evidence.UnavailableUntil != nil && !evidence.UnavailableUntil.After(now))
	cooling := evidence.CooldownRetryAt != nil && evidence.CooldownRetryAt.After(now)
	allowanceBlocked := len(ExhaustedResets(evidence.Allowance, now, false, "")) > 0
	blocked := healthBlocked || cooling || allowanceBlocked
	deadline := func() *time.Time {
		if !blocked || healthBlocked && evidence.UnavailableUntil == nil {
			return nil
		}
		unavailableUntil := func() *time.Time {
			if healthBlocking {
				return evidence.UnavailableUntil
			}
			return nil
		}()
		return BlockingDeadline(now, unavailableUntil, evidence.CooldownRetryAt, evidence.Allowance, false, "")
	}()
	exhausted := exhaustedAllowanceEvidences(evidence.Allowance)
	blockerObserved := latestTime(slices.Concat(
		[]*time.Time{evidence.CooldownObservedAt},
		slices.Collect(func(yield func(*time.Time) bool) {
			if healthBlocking && !yield(evidence.HealthObservedAt) {
				return
			}
			for _, item := range exhausted {
				observed := item.observed
				if !yield(&observed) {
					return
				}
			}
		}),
	)...)
	passedDeadline := latestTime(slices.Collect(func(yield func(*time.Time) bool) {
		if healthBlocking && evidence.UnavailableUntil != nil && !evidence.UnavailableUntil.After(now) && !yield(evidence.UnavailableUntil) {
			return
		}
		if evidence.CooldownRetryAt != nil && !evidence.CooldownRetryAt.After(now) && !yield(evidence.CooldownRetryAt) {
			return
		}
		for _, item := range exhausted {
			if item.reset != nil && !item.reset.After(now) && !yield(item.reset) {
				return
			}
		}
	})...)
	positive := evidence.Health == "healthy" && evidence.HealthObservedAt != nil &&
		(blockerObserved == nil || evidence.HealthObservedAt.After(*blockerObserved)) &&
		(passedDeadline == nil || !evidence.HealthObservedAt.Before(*passedDeadline))
	allowanceObserved := func() *time.Time {
		if evidence.Allowance == nil || evidence.Allowance.ObservedAt.IsZero() {
			return nil
		}
		return &evidence.Allowance.ObservedAt
	}()
	return threadAccountAssessment{
		blocked:      blocked,
		deadline:     deadline,
		positive:     positive,
		lastObserved: latestTime(evidence.HealthObservedAt, evidence.CooldownObservedAt, allowanceObserved),
	}
}

func AssessThreadStatus(now time.Time, retired bool, accounts []ThreadAccountEvidence) ThreadStatus {
	assessments := slices.Collect(func(yield func(threadAccountAssessment) bool) {
		for _, account := range accounts {
			if !yield(assessThreadAccount(now, account)) {
				return
			}
		}
	})
	lastObserved := latestTime(slices.Collect(func(yield func(*time.Time) bool) {
		for _, assessment := range assessments {
			if !yield(assessment.lastObserved) {
				return
			}
		}
	})...)
	unavailable := retired ||
		slices.ContainsFunc(accounts, func(account ThreadAccountEvidence) bool { return !account.Eligible }) ||
		slices.ContainsFunc(assessments, func(assessment threadAccountAssessment) bool { return assessment.blocked })
	if unavailable {
		retryAt := func() *time.Time {
			if retired || slices.ContainsFunc(accounts, func(account ThreadAccountEvidence) bool { return !account.Eligible }) {
				return nil
			}
			if slices.ContainsFunc(assessments, func(assessment threadAccountAssessment) bool {
				return assessment.blocked && assessment.deadline == nil
			}) {
				return nil
			}
			return latestTime(slices.Collect(func(yield func(*time.Time) bool) {
				for _, assessment := range assessments {
					if assessment.blocked && !yield(assessment.deadline) {
						return
					}
				}
			})...)
		}()
		return ThreadStatus{Status: ThreadUnavailable, CheckedAt: now, LastObservedAt: lastObserved, RetryAt: retryAt}
	}
	if len(assessments) > 0 && !slices.ContainsFunc(assessments, func(assessment threadAccountAssessment) bool { return !assessment.positive }) {
		return ThreadStatus{Status: ThreadAvailable, CheckedAt: now, LastObservedAt: lastObserved}
	}
	return ThreadStatus{Status: ThreadUnknown, CheckedAt: now, LastObservedAt: lastObserved}
}
