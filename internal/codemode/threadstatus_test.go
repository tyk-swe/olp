package codemode_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func threadStatusTime(hour, minute int) time.Time {
	return time.Date(2026, 11, 5, hour, minute, 0, 0, time.UTC)
}

func coolingThreadEvidence() codemode.ThreadAccountEvidence {
	observed := threadStatusTime(10, 0)
	retry := threadStatusTime(10, 5)
	return codemode.ThreadAccountEvidence{
		Eligible:           true,
		Health:             "unavailable",
		HealthObservedAt:   &observed,
		UnavailableUntil:   &retry,
		CooldownRetryAt:    &retry,
		CooldownObservedAt: &observed,
	}
}

func TestThreadStatusCooldownShowsRetryThenUnknown(t *testing.T) {
	observed := threadStatusTime(10, 0)
	retry := threadStatusTime(10, 5)
	before := codemode.AssessThreadStatus(threadStatusTime(10, 2), false, []codemode.ThreadAccountEvidence{coolingThreadEvidence()})
	if before.Status != codemode.ThreadUnavailable || before.RetryAt == nil || !before.RetryAt.Equal(retry) ||
		before.LastObservedAt == nil || !before.LastObservedAt.Equal(observed) || !before.CheckedAt.Equal(threadStatusTime(10, 2)) {
		t.Fatalf("status at 10:02 = %+v, want unavailable with retry at 10:05 and last observed 10:00", before)
	}
	after := codemode.AssessThreadStatus(threadStatusTime(10, 6), false, []codemode.ThreadAccountEvidence{coolingThreadEvidence()})
	if after.Status != codemode.ThreadUnknown || after.RetryAt != nil ||
		after.LastObservedAt == nil || !after.LastObservedAt.Equal(observed) || !after.CheckedAt.Equal(threadStatusTime(10, 6)) {
		t.Fatalf("status at 10:06 = %+v, want unknown with no retry time and last observed 10:00", after)
	}
}

func TestThreadStatusNeedsPositiveEvidenceForAvailable(t *testing.T) {
	healthyAt := threadStatusTime(10, 7)
	recovered := codemode.ThreadAccountEvidence{Eligible: true, Health: "healthy", HealthObservedAt: &healthyAt, CooldownRetryAt: coolingThreadEvidence().CooldownRetryAt, CooldownObservedAt: coolingThreadEvidence().CooldownObservedAt}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 8), false, []codemode.ThreadAccountEvidence{recovered}); got.Status != codemode.ThreadAvailable || got.RetryAt != nil {
		t.Fatalf("recovered status = %+v, want available", got)
	}
	earlyHealthy := threadStatusTime(10, 3)
	otherModel := codemode.ThreadAccountEvidence{Eligible: true, Health: "healthy", HealthObservedAt: &earlyHealthy, CooldownRetryAt: coolingThreadEvidence().CooldownRetryAt, CooldownObservedAt: coolingThreadEvidence().CooldownObservedAt}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 6), false, []codemode.ThreadAccountEvidence{otherModel}); got.Status != codemode.ThreadUnknown || got.RetryAt != nil {
		t.Fatalf("status with health observed before the deadline passed = %+v, want unknown", got)
	}
	neverObserved := codemode.ThreadAccountEvidence{Eligible: true, Health: "unknown"}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 2), false, []codemode.ThreadAccountEvidence{neverObserved}); got.Status != codemode.ThreadUnknown || got.LastObservedAt != nil || got.RetryAt != nil {
		t.Fatalf("unobserved status = %+v, want unknown without observation or retry times", got)
	}
	steadyAt := threadStatusTime(9, 30)
	steady := codemode.ThreadAccountEvidence{Eligible: true, Health: "healthy", HealthObservedAt: &steadyAt}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 2), false, []codemode.ThreadAccountEvidence{steady}); got.Status != codemode.ThreadAvailable {
		t.Fatalf("steady status = %+v, want available", got)
	}
}

func TestThreadStatusBlockersWithoutKnownDeadlineHaveNoRetry(t *testing.T) {
	observed := threadStatusTime(10, 0)
	noDeadline := codemode.ThreadAccountEvidence{Eligible: true, Health: "quota_limited", HealthObservedAt: &observed}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 2), false, []codemode.ThreadAccountEvidence{noDeadline}); got.Status != codemode.ThreadUnavailable || got.RetryAt != nil {
		t.Fatalf("quota status = %+v, want unavailable without a retry time", got)
	}
	exhausted := codemode.ThreadAccountEvidence{Eligible: true, Health: "healthy", HealthObservedAt: &observed, Allowance: &codemode.Allowance{
		Windows:    []codemode.AllowanceWindow{{LimitID: "codex", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidencePassive, UsedPercent: 100, RemainingPercent: 0, ObservedAt: observed}},
		ObservedAt: observed,
	}}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 2), false, []codemode.ThreadAccountEvidence{exhausted}); got.Status != codemode.ThreadUnavailable || got.RetryAt != nil {
		t.Fatalf("exhausted status = %+v, want unavailable without a retry time", got)
	}
	reset := threadStatusTime(10, 5)
	exhausted.Allowance.Windows[0].ResetsAt = &reset
	exhausted.Allowance.ResetsAt = &reset
	remaining := float64(0)
	exhausted.Allowance.RemainingPercent = &remaining
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 2), false, []codemode.ThreadAccountEvidence{exhausted}); got.Status != codemode.ThreadUnavailable || got.RetryAt == nil || !got.RetryAt.Equal(reset) {
		t.Fatalf("exhausted status with reset = %+v, want unavailable with retry at the reset", got)
	}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 6), false, []codemode.ThreadAccountEvidence{exhausted}); got.Status != codemode.ThreadUnknown || got.RetryAt != nil {
		t.Fatalf("exhausted status after reset = %+v, want unknown without a retry time", got)
	}
}

func TestThreadStatusAggregatesTreeAccounts(t *testing.T) {
	healthyAt := threadStatusTime(9, 30)
	healthy := codemode.ThreadAccountEvidence{Eligible: true, Health: "healthy", HealthObservedAt: &healthyAt}
	retry := threadStatusTime(10, 5)
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 2), false, []codemode.ThreadAccountEvidence{healthy, coolingThreadEvidence()}); got.Status != codemode.ThreadUnavailable || got.RetryAt == nil || !got.RetryAt.Equal(retry) {
		t.Fatalf("mixed tree status = %+v, want unavailable until the blocked account clears", got)
	}
	unobserved := codemode.ThreadAccountEvidence{Eligible: true, Health: "unknown"}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 2), false, []codemode.ThreadAccountEvidence{healthy, unobserved}); got.Status != codemode.ThreadUnknown {
		t.Fatalf("tree with unobserved account = %+v, want unknown", got)
	}
	ineligible := codemode.ThreadAccountEvidence{Eligible: false, Health: "healthy", HealthObservedAt: &healthyAt}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 2), false, []codemode.ThreadAccountEvidence{ineligible}); got.Status != codemode.ThreadUnavailable || got.RetryAt != nil {
		t.Fatalf("ineligible status = %+v, want unavailable without a retry time", got)
	}
	if got := codemode.AssessThreadStatus(threadStatusTime(10, 2), true, []codemode.ThreadAccountEvidence{healthy}); got.Status != codemode.ThreadUnavailable || got.RetryAt != nil {
		t.Fatalf("retired status = %+v, want unavailable without a retry time", got)
	}
}
