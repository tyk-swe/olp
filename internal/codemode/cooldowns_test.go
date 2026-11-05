package codemode_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

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
	early := bandNow.Add(10 * time.Minute)
	middle := bandNow.Add(45 * time.Minute)
	unavailable := bandNow.Add(time.Hour)
	late := bandNow.Add(2 * time.Hour)
	exhausted := percentWindow(0, &late, bandNow)
	healthy := percentWindow(80, &early, bandNow)
	healthy.Window = "secondary"
	low := percentWindow(5, &early, bandNow)
	low.Window = "five_hour"
	allowance := bandAllowance(exhausted, healthy, low)
	cooldownRetry := bandNow.Add(30 * time.Minute)
	deadline := codemode.BlockingDeadline(bandNow, &unavailable, &cooldownRetry, allowance)
	if deadline == nil || !deadline.Equal(late) {
		t.Fatalf("BlockingDeadline = %v, want %v", deadline, late)
	}
	other := codemode.BlockingDeadline(bandNow, nil, &middle, bandAllowance(percentWindow(80, &early, bandNow)))
	if other == nil || !other.Equal(middle) {
		t.Fatalf("BlockingDeadline(other) = %v, want %v", other, middle)
	}
	if got := codemode.EarliestRecovery(bandNow, deadline, other); got == nil || !got.Equal(middle) {
		t.Fatalf("EarliestRecovery = %v, want %v", got, middle)
	}
}

func TestExhaustedResetsOnlyFreshExhaustedWindows(t *testing.T) {
	future := bandNow.Add(time.Hour)
	early := bandNow.Add(10 * time.Minute)
	staleReset := bandNow.Add(-2 * time.Minute)
	stale := percentWindow(0, &staleReset, bandNow.Add(-5*time.Minute))
	stale.Window = "secondary"
	allowance := bandAllowance(percentWindow(0, &future, bandNow), percentWindow(5, &early, bandNow), stale)
	allowance.Windows[1].Window = "five_hour"
	resets := codemode.ExhaustedResets(allowance, bandNow)
	if len(resets) != 1 || resets[0] == nil || !resets[0].Equal(future) {
		t.Fatalf("ExhaustedResets = %v, want only %v", resets, future)
	}
	if got := codemode.BlockingDeadline(bandNow, nil, nil, bandAllowance(percentWindow(80, &early, bandNow))); got != nil {
		t.Fatalf("healthy-only deadline = %v, want nil", got)
	}
	past := bandNow.Add(-time.Hour)
	if got := codemode.BlockingDeadline(bandNow, &past, &past, nil); got != nil {
		t.Fatalf("past-only deadline = %v, want nil", got)
	}
}
