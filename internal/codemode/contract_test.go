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
