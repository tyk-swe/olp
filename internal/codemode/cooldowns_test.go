package codemode_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

var allowanceNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func percentWindow(remaining float64, resetsAt *time.Time, observedAt time.Time) codemode.AllowanceWindow {
	return codemode.AllowanceWindow{LimitID: "codex", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidencePassive, UsedPercent: 100 - remaining, RemainingPercent: remaining, ResetsAt: resetsAt, ObservedAt: observedAt}
}

func allowanceOf(windows ...codemode.AllowanceWindow) *codemode.Allowance {
	return &codemode.Allowance{Windows: windows, ObservedAt: allowanceNow}
}

func TestEarliestRecoveryPicksSoonestFutureReset(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	soon := now.Add(10 * time.Minute)
	later := now.Add(time.Hour)
	got := codemode.EarliestRecovery(now, &later, &past, nil, &soon)
	if got == nil || !got.Equal(soon) {
		t.Fatalf("EarliestRecovery = %v", got)
	}
	if got := codemode.EarliestRecovery(now, &past, nil); got != nil {
		t.Fatalf("EarliestRecovery(past) = %v", got)
	}

}

func TestRefusalRetryAfterRoundsUpWithOneSecondFloor(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	soon := now.Add(250 * time.Millisecond)
	if got := (&codemode.Refusal{Status: 503, Code: "code_account_unavailable", RetryAt: &soon}).RetryAfter(now); got != time.Second {
		t.Fatalf("RetryAfter(250ms) = %s", got)
	}
	partial := now.Add(90*time.Second + time.Millisecond)
	if got := (&codemode.Refusal{RetryAt: &partial}).RetryAfter(now); got != 91*time.Second {
		t.Fatalf("RetryAfter(90s+1ms) = %s", got)
	}
	past := now.Add(-time.Second)
	if got := (&codemode.Refusal{RetryAt: &past}).RetryAfter(now); got != 0 {
		t.Fatalf("RetryAfter(past) = %s", got)
	}
	if got := (&codemode.Refusal{}).RetryAfter(now); got != 0 {
		t.Fatalf("RetryAfter(unset) = %s", got)
	}
	var missing *codemode.Refusal
	if got := missing.RetryAfter(now); got != 0 {
		t.Fatalf("RetryAfter(nil) = %s", got)
	}
}

func TestBlockingDeadlineIgnoresNonBlockingEarlierResets(t *testing.T) {
	early := allowanceNow.Add(10 * time.Minute)
	middle := allowanceNow.Add(45 * time.Minute)
	unavailable := allowanceNow.Add(time.Hour)
	late := allowanceNow.Add(2 * time.Hour)
	exhausted := percentWindow(0, &late, allowanceNow)
	healthy := percentWindow(80, &early, allowanceNow)
	healthy.Window = "secondary"
	low := percentWindow(5, &early, allowanceNow)
	low.Window = "five_hour"
	allowance := allowanceOf(exhausted, healthy, low)
	cooldownRetry := allowanceNow.Add(30 * time.Minute)
	deadline := codemode.BlockingDeadline(allowanceNow, &unavailable, &cooldownRetry, allowance)
	if deadline == nil || !deadline.Equal(late) {
		t.Fatalf("BlockingDeadline = %v, want %v", deadline, late)
	}
	other := codemode.BlockingDeadline(allowanceNow, nil, &middle, allowanceOf(percentWindow(80, &early, allowanceNow)))
	if other == nil || !other.Equal(middle) {
		t.Fatalf("BlockingDeadline(other) = %v, want %v", other, middle)
	}
	if got := codemode.EarliestRecovery(allowanceNow, deadline, other); got == nil || !got.Equal(middle) {
		t.Fatalf("EarliestRecovery = %v, want %v", got, middle)
	}
}

func TestExhaustedResetsOnlyFreshExhaustedWindows(t *testing.T) {
	future := allowanceNow.Add(time.Hour)
	early := allowanceNow.Add(10 * time.Minute)
	staleReset := allowanceNow.Add(-2 * time.Minute)
	stale := percentWindow(0, &staleReset, allowanceNow.Add(-5*time.Minute))
	stale.Window = "secondary"
	allowance := allowanceOf(percentWindow(0, &future, allowanceNow), percentWindow(5, &early, allowanceNow), stale)
	allowance.Windows[1].Window = "five_hour"
	resets := codemode.ExhaustedResets(allowance, allowanceNow)
	if len(resets) != 1 || resets[0] == nil || !resets[0].Equal(future) {
		t.Fatalf("ExhaustedResets = %v, want only %v", resets, future)
	}
	if got := codemode.BlockingDeadline(allowanceNow, nil, nil, allowanceOf(percentWindow(80, &early, allowanceNow))); got != nil {
		t.Fatalf("healthy-only deadline = %v, want nil", got)
	}
	past := allowanceNow.Add(-time.Hour)
	if got := codemode.BlockingDeadline(allowanceNow, &past, &past, nil); got != nil {
		t.Fatalf("past-only deadline = %v, want nil", got)
	}
}
