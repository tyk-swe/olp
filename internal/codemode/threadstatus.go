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

type threadBlocker struct {
	observedAt *time.Time
	deadline   *time.Time
}

func (b threadBlocker) active(now time.Time) bool {
	return b.deadline == nil || b.deadline.After(now)
}

func (b threadBlocker) outdates(healthy, now time.Time) bool {
	return b.observedAt != nil && !healthy.After(*b.observedAt) || !b.active(now) && healthy.Before(*b.deadline)
}

func threadBlockers(evidence ThreadAccountEvidence) []threadBlocker {
	return slices.Collect(func(yield func(threadBlocker) bool) {
		if (evidence.Health == "unavailable" || evidence.Health == "quota_limited") &&
			!yield(threadBlocker{observedAt: evidence.HealthObservedAt, deadline: evidence.UnavailableUntil}) {
			return
		}
		if evidence.CooldownRetryAt != nil && !yield(threadBlocker{observedAt: evidence.CooldownObservedAt, deadline: evidence.CooldownRetryAt}) {
			return
		}
		for exhaustion := range allowanceExhaustions(evidence.Allowance) {
			if !yield(threadBlocker{observedAt: &exhaustion.observedAt, deadline: exhaustion.resetsAt}) {
				return
			}
		}
	})
}

type threadAccountAssessment struct {
	blocking     []*time.Time
	positive     bool
	lastObserved *time.Time
}

func assessThreadAccount(now time.Time, evidence ThreadAccountEvidence) threadAccountAssessment {
	blockers := threadBlockers(evidence)
	allowanceObserved := func() *time.Time {
		if evidence.Allowance == nil {
			return nil
		}
		return &evidence.Allowance.ObservedAt
	}()
	return threadAccountAssessment{
		blocking: slices.Collect(func(yield func(*time.Time) bool) {
			for _, blocker := range blockers {
				if blocker.active(now) && !yield(blocker.deadline) {
					return
				}
			}
		}),
		positive: evidence.Health == "healthy" && evidence.HealthObservedAt != nil &&
			!slices.ContainsFunc(blockers, func(blocker threadBlocker) bool { return blocker.outdates(*evidence.HealthObservedAt, now) }),
		lastObserved: latestTime(evidence.HealthObservedAt, evidence.CooldownObservedAt, allowanceObserved),
	}
}

func AssessThreadStatus(now time.Time, bindingUnavailable bool, accounts []ThreadAccountEvidence) ThreadStatus {
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
	blocking := slices.Concat(slices.Collect(func(yield func([]*time.Time) bool) {
		for _, assessment := range assessments {
			if !yield(assessment.blocking) {
				return
			}
		}
	})...)
	switch {
	case bindingUnavailable || slices.ContainsFunc(accounts, func(account ThreadAccountEvidence) bool { return !account.Eligible }):
		return ThreadStatus{Status: ThreadUnavailable, CheckedAt: now, LastObservedAt: lastObserved}
	case len(blocking) > 0:
		return ThreadStatus{Status: ThreadUnavailable, CheckedAt: now, LastObservedAt: lastObserved, RetryAt: knownDeadline(blocking)}
	case len(assessments) > 0 && !slices.ContainsFunc(assessments, func(assessment threadAccountAssessment) bool { return !assessment.positive }):
		return ThreadStatus{Status: ThreadAvailable, CheckedAt: now, LastObservedAt: lastObserved}
	}
	return ThreadStatus{Status: ThreadUnknown, CheckedAt: now, LastObservedAt: lastObserved}
}

func knownDeadline(deadlines []*time.Time) *time.Time {
	if slices.Contains(deadlines, nil) {
		return nil
	}
	return latestTime(deadlines...)
}

func latestTime(times ...*time.Time) *time.Time {
	known := slices.Collect(func(yield func(time.Time) bool) {
		for _, candidate := range times {
			if candidate != nil && !candidate.IsZero() && !yield(*candidate) {
				return
			}
		}
	})
	if len(known) == 0 {
		return nil
	}
	latest := slices.MaxFunc(known, time.Time.Compare)
	return &latest
}
