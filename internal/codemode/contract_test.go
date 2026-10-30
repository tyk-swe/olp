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

func TestRouteExactAliasesRequireExplicitRewriting(t *testing.T) {
	for _, test := range []struct {
		name  string
		route Route
		valid bool
	}{
		{"native default", Route{Models: []string{"native-a"}}, true},
		{"exact alias", Route{Models: []string{"native-a"}, ModelRewriting: true, Aliases: map[string]string{"coding": "native-a"}}, true},
		{"missing opt in", Route{Models: []string{"native-a"}, Aliases: map[string]string{"coding": "native-a"}}, false},
		{"unknown target", Route{Models: []string{"native-a"}, ModelRewriting: true, Aliases: map[string]string{"coding": "native-b"}}, false},
		{"native collision", Route{Models: []string{"native-a"}, ModelRewriting: true, Aliases: map[string]string{"native-a": "native-a"}}, false},
		{"alias chain", Route{Models: []string{"native-a"}, ModelRewriting: true, Aliases: map[string]string{"coding": "other", "other": "native-a"}}, false},
		{"invalid name", Route{Models: []string{"native-a"}, ModelRewriting: true, Aliases: map[string]string{"bad name": "native-a"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.route.ValidateAliases(); (err == nil) != test.valid {
				t.Fatalf("alias policy validity %v, error %v", test.valid, err)
			}
		})
	}
	route := Route{Models: []string{"native-a", "native-b"}, ModelRewriting: true, Aliases: map[string]string{"coding": "native-a"}}
	alias, err := route.ResolveModel(Operation{Model: "coding"})
	if err != nil || alias.Model != "native-a" || alias.PublicModel() != "coding" {
		t.Fatalf("exact alias resolution %+v, %v", alias, err)
	}
	native, err := route.ResolveModel(Operation{Model: "native-b"})
	if err != nil || native.Model != "native-b" || native.PublicModel() != "native-b" {
		t.Fatalf("native resolution %+v, %v", native, err)
	}
	if _, err := route.ResolveModel(Operation{Model: "unknown"}); err == nil {
		t.Fatal("unpublished model accepted")
	}
}

func TestAliasRepublishKeepsExistingExactModelPins(t *testing.T) {
	route := Route{Models: []string{"native-a", "native-b"}, Aliases: map[string]string{"team-code": "native-b"}, ModelRewriting: true}
	operation := Operation{Name: "responses.create", Model: "team-code", Identity: Identity{Conversation: "continued"}}
	pins := []Pin{{Model: "team-code", TargetModel: "native-a", AccountID: "original"}, {Model: "native-b", AccountID: "native"}}
	continued, err := route.ResolvePinnedModel(operation, pins)
	if err != nil || continued.Model != "native-a" || continued.PublicModel() != operation.Model || continued.Identity != operation.Identity {
		t.Fatalf("republished alias moved existing conversation %+v, %v", continued, err)
	}
	fresh, err := route.ResolvePinnedModel(operation, nil)
	if err != nil || fresh.Model != "native-b" || fresh.PublicModel() != operation.Model {
		t.Fatalf("new conversation did not use current target %+v, %v", fresh, err)
	}
	native, err := route.ResolvePinnedModel(Operation{Model: "native-b"}, pins)
	if err != nil || native.Model != "native-b" || native.RequestedModel != "" {
		t.Fatalf("existing native pin changed %+v, %v", native, err)
	}
	removed := Route{Models: []string{"native-b"}, Aliases: route.Aliases, ModelRewriting: true}
	if _, err := removed.ResolvePinnedModel(operation, pins); err == nil {
		t.Fatal("removed target silently moved existing alias pin")
	}
	if _, err := (Route{Models: route.Models}).ResolvePinnedModel(operation, pins); err == nil {
		t.Fatal("removed alias kept serving old pin")
	}
}

func TestPaidTargetsRequireExplicitParticipationAndExactNativeModel(t *testing.T) {
	const provider = "019a06bf-341f-7000-8000-000000000001"
	const slot = "019a06bf-341f-7000-8000-000000000002"
	valid := Route{Models: []string{"native-model"}, PaidParticipation: true, PaidTargets: map[string]PaidTarget{"native-model": {ProviderID: provider, SlotID: slot}}}
	if err := valid.ValidatePaidTargets(); err != nil {
		t.Fatal(err)
	}
	for _, route := range []Route{
		{Models: valid.Models, PaidTargets: valid.PaidTargets},
		{Models: valid.Models, PaidParticipation: true, PaidTargets: map[string]PaidTarget{"other": {ProviderID: provider, SlotID: slot}}},
		{Models: valid.Models, PaidParticipation: true, PaidTargets: map[string]PaidTarget{"native-model": {ProviderID: provider}}},
	} {
		if err := route.ValidatePaidTargets(); err == nil {
			t.Fatal("invalid paid participation accepted")
		}
	}
}

func TestModelPoolsKeepExactDefaultsAndPermitOnlyPublishedAlternates(t *testing.T) {
	operation := Operation{Name: "responses", Model: "team-code", Identity: Identity{Conversation: "recovery"}}
	route := Route{Models: []string{"native-a", "native-b"}, Aliases: map[string]string{"team-code": "native-a"}, ModelRewriting: true, InitialRecoveryAttempts: 1}
	pins := []Pin{{Model: "team-code", TargetModel: "native-a"}}
	exact, err := route.RecoveryModels(operation, pins)
	if err != nil || len(exact) != 1 || exact[0].Model != "native-a" || exact[0].PublicModel() != "team-code" {
		t.Fatalf("exact default changed %+v, %v", exact, err)
	}
	pooled := Route{Models: route.Models, Aliases: route.Aliases, ModelRewriting: route.ModelRewriting, InitialRecoveryAttempts: route.InitialRecoveryAttempts,
		ModelPools: map[string][]string{"team-code": {"native-a", "native-b"}}}
	if err := pooled.ValidateModelPools(); err != nil {
		t.Fatal(err)
	}
	alternates, err := pooled.RecoveryModels(operation, pins)
	if err != nil || len(alternates) != 2 || alternates[1].Model != "native-b" || alternates[1].PublicModel() != "team-code" || alternates[1].Identity != operation.Identity {
		t.Fatalf("explicit pool lost target or public identity %+v, %v", alternates, err)
	}
	continued, err := pooled.ResolvePinnedModel(operation, []Pin{{Model: "team-code", TargetModel: "native-b"}})
	if err != nil || continued.Model != "native-b" || continued.PublicModel() != "team-code" {
		t.Fatalf("recovered model did not stay pinned %+v, %v", continued, err)
	}
	nativePool := Route{Models: route.Models, ModelRewriting: true, ModelPools: map[string][]string{"native-a": {"native-a", "native-b"}}}
	nativeSelected, nativeErr := nativePool.ResolvePinnedModel(Operation{Model: "native-a"}, []Pin{{Model: "native-a", TargetModel: "native-b"}})
	if nativeErr != nil || nativeSelected.Model != "native-b" || nativeSelected.PublicModel() != "native-a" {
		t.Fatalf("native pool lost requested model %+v, %v", nativeSelected, nativeErr)
	}
	for _, invalid := range []Route{
		{Models: route.Models, ModelPools: map[string][]string{"native-a": {"native-a", "native-b"}}},
		{Models: route.Models, ModelRewriting: true, ModelPools: map[string][]string{"unknown": {"native-a", "native-b"}}},
		{Models: route.Models, ModelRewriting: true, ModelPools: map[string][]string{"native-a": {"native-b", "native-a"}}},
		{Models: route.Models, ModelRewriting: true, ModelPools: map[string][]string{"native-a": {"native-a", "unpublished"}}},
		{Models: route.Models, ModelRewriting: true, ModelPools: map[string][]string{"native-a": {"native-a", "native-a"}}},
	} {
		if err := invalid.ValidateModelPools(); err == nil {
			t.Fatalf("invalid pool accepted %+v", invalid)
		}
	}
}
