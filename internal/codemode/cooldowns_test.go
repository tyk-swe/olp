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

func TestLatestDeadlineUsesLatestActiveRestriction(t *testing.T) {
	middle := bandNow.Add(45 * time.Minute)
	unavailable := bandNow.Add(time.Hour)
	cooldownRetry := bandNow.Add(30 * time.Minute)
	deadline := codemode.LatestDeadline(bandNow, &unavailable, &cooldownRetry)
	if deadline == nil || !deadline.Equal(unavailable) {
		t.Fatalf("LatestDeadline = %v, want %v", deadline, unavailable)
	}
	other := codemode.LatestDeadline(bandNow, nil, &middle)
	if other == nil || !other.Equal(middle) {
		t.Fatalf("LatestDeadline(other) = %v, want %v", other, middle)
	}
	if got := codemode.EarliestRecovery(bandNow, deadline, other); got == nil || !got.Equal(middle) {
		t.Fatalf("EarliestRecovery = %v, want %v", got, middle)
	}
	past := bandNow.Add(-time.Hour)
	if got := codemode.LatestDeadline(bandNow, &past, nil); got != nil {
		t.Fatalf("past-only deadline = %v, want nil", got)
	}
}
