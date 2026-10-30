package codemode

import (
	"errors"
	"testing"
	"time"
)

func TestValidateModelsRejectsDuplicates(t *testing.T) {
	if err := ValidateModels([]string{"glm-5.3", "glm-5.3"}); err == nil {
		t.Fatal("duplicate models accepted")
	}
	if err := ValidateModels([]string{"glm-5.3", "gpt-5.5"}); err != nil {
		t.Fatal(err)
	}
}

func TestUsageValidateKeepsBreakdownsAsSubsets(t *testing.T) {
	total, input, output, cached, reasoning := int64(10), int64(6), int64(4), int64(7), int64(1)
	if err := (Usage{Total: &total, Input: &input, Output: &output, Cached: &cached}).Validate(); err == nil {
		t.Fatal("cached usage above input accepted")
	}
	cached = 6
	if err := (Usage{Total: &total, Input: &input, Output: &output, Cached: &cached, Reasoning: &reasoning}).Validate(); err != nil {
		t.Fatal(err)
	}
	total = 11
	if err := (Usage{Total: &total, Input: &input, Output: &output}).Validate(); err == nil {
		t.Fatal("input plus output differing from total accepted")
	}
}

func TestIdentityValidateRefusesSelfParent(t *testing.T) {
	err := (Identity{Conversation: "ses-1", Parent: "ses-1"}).Validate()
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Code != "code_identity_invalid" {
		t.Fatalf("self-parent identity: %v", err)
	}
}

func TestAllowanceNormalizeMirrorsPrimaryWindow(t *testing.T) {
	reset := time.Unix(1_700_000_000, 0).UTC()
	allowance := Allowance{Windows: []AllowanceWindow{
		{LimitID: "codex", Window: "secondary", UsedPercent: 10, RemainingPercent: 90, ObservedAt: reset},
		{LimitID: "codex", Window: "primary", UsedPercent: 25, RemainingPercent: 75, ResetsAt: &reset, ObservedAt: reset},
	}}
	allowance.Normalize()
	if allowance.Windows[0].Window != "primary" {
		t.Fatalf("windows not ordered: %+v", allowance.Windows)
	}
	if allowance.RemainingPercent == nil || *allowance.RemainingPercent != 75 || allowance.ResetsAt == nil || !allowance.ResetsAt.Equal(reset) {
		t.Fatalf("primary window not mirrored: %+v", allowance)
	}
}

func TestTokenBoundRequiresQualifiedEvidence(t *testing.T) {
	var bound *TokenBound
	err := bound.Validate()
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Code != "code_token_bound_unavailable" {
		t.Fatalf("missing bound: %v", err)
	}
}

func TestAllowanceMergeRetainsFreshPartialObservationsAndApplicableResets(t *testing.T) {
	observed := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	reset := observed.Add(time.Hour)
	current := Allowance{
		RemainingTokens: new(int64(0)), RemainingRequests: new(int64(7)), ObservedAt: observed,
		TokenObservation: &CountObservation{ObservedAt: observed, ResetsAt: &reset},
		Windows:          []AllowanceWindow{{LimitID: "codex", Window: "primary", UsedPercent: 100, RemainingPercent: 0, ResetsAt: &reset, ObservedAt: observed}},
		Credits:          &Credits{HasCredits: true, ObservedAt: observed},
	}
	partial := Allowance{
		RemainingTokens: new(int64(0)), ObservedAt: observed.Add(time.Minute),
		Windows: []AllowanceWindow{{LimitID: "codex", Window: "primary", UsedPercent: 100, RemainingPercent: 0, ObservedAt: observed.Add(time.Minute)}},
	}
	merged := current.Merge(partial)
	stale := Allowance{
		RemainingTokens: new(int64(99)), RemainingRequests: new(int64(99)), ObservedAt: observed.Add(-time.Minute),
		Windows: []AllowanceWindow{{LimitID: "codex", Window: "primary", UsedPercent: 1, RemainingPercent: 99, ObservedAt: observed.Add(-time.Minute)}},
		Credits: &Credits{HasCredits: false, ObservedAt: observed.Add(-time.Minute)},
	}
	result := merged.Merge(stale)
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if *result.RemainingTokens != 0 || *result.RemainingRequests != 7 || !result.Credits.HasCredits || *result.RemainingPercent != 0 {
		t.Fatalf("stale or partial observation replaced fresh values %+v", result)
	}
	if result.TokenObservation.ResetsAt == nil || !result.TokenObservation.ResetsAt.Equal(reset) || result.ResetsAt == nil || !result.ResetsAt.Equal(reset) {
		t.Fatalf("partial observation discarded an applicable reset %+v", result)
	}
	afterReset := Allowance{
		RemainingTokens: new(int64(0)), ObservedAt: reset.Add(time.Minute),
		Windows: []AllowanceWindow{{LimitID: "codex", Window: "primary", UsedPercent: 100, RemainingPercent: 0, ObservedAt: reset.Add(time.Minute)}},
	}
	exhausted := result.Merge(afterReset)
	if exhausted.TokenObservation.ResetsAt != nil || exhausted.ResetsAt != nil || *exhausted.RemainingTokens != 0 || *exhausted.RemainingPercent != 0 {
		t.Fatalf("expired reset invented recovery for a fresh exhaustion %+v", exhausted)
	}
	if !current.ObservedAt.Equal(observed) || current.Windows[0].ResetsAt == nil || !current.Windows[0].ResetsAt.Equal(reset) {
		t.Fatal("merging changed the original observation")
	}
}

func TestAllowanceMergeUsesIndependentObservationTimes(t *testing.T) {
	observed := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	current := (Allowance{}).Merge(Allowance{
		RemainingTokens: new(int64(8)), RemainingRequests: new(int64(4)), ObservedAt: observed.Add(10 * time.Minute),
		TokenObservation: &CountObservation{ObservedAt: observed.Add(10 * time.Minute)}, RequestObservation: &CountObservation{ObservedAt: observed},
		RemainingPercent: new(float64(20)),
		Credits:          &Credits{HasCredits: true, ObservedAt: observed},
	})
	updated := current.Merge(Allowance{
		RemainingTokens: new(int64(2)), RemainingRequests: new(int64(0)), ObservedAt: observed.Add(5 * time.Minute),
		Windows: []AllowanceWindow{{LimitID: "codex", Window: "secondary", UsedPercent: 100, RemainingPercent: 0, ObservedAt: observed.Add(5 * time.Minute)}},
		Credits: &Credits{HasCredits: false, ObservedAt: observed.Add(5 * time.Minute)},
	})
	if err := updated.Validate(); err != nil {
		t.Fatal(err)
	}
	if *updated.RemainingTokens != 8 || *updated.RemainingRequests != 0 || *updated.RemainingPercent != 20 || updated.Credits.HasCredits || !updated.ObservedAt.Equal(current.ObservedAt) || len(updated.Windows) != 2 {
		t.Fatalf("aggregate timestamp overrode independently observed fields %+v", updated)
	}
}

func TestHealthObservationRequiresTimestampAndCompatibleRetry(t *testing.T) {
	observed := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		observation HealthObservation
		valid       bool
	}{
		{"healthy", HealthObservation{Status: "healthy", ObservedAt: observed}, true},
		{"failure", HealthObservation{Status: "unavailable", ObservedAt: observed, RetryAt: new(observed.Add(time.Minute))}, true},
		{"quota without signal", HealthObservation{Status: "quota_limited", ObservedAt: observed}, true},
		{"missing time", HealthObservation{Status: "unavailable"}, false},
		{"unknown status", HealthObservation{Status: "unknown", ObservedAt: observed}, false},
		{"healthy cooldown", HealthObservation{Status: "healthy", ObservedAt: observed, RetryAt: new(observed.Add(time.Minute))}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.observation.Validate(); (err == nil) != test.valid {
				t.Fatalf("health validity %v, error %v", test.valid, err)
			}
		})
	}
}
