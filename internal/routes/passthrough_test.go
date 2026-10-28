package routes

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func passthroughDraft(t *testing.T, behavior routing.Behavior) *draft {
	t.Helper()
	encoded, err := json.Marshal(behavior)
	if err != nil {
		t.Fatal(err)
	}
	fidelity, _ := json.Marshal(routing.RouteFidelity{Mode: routing.FidelityStrict})
	return &draft{
		ID: "11111111-1111-1111-1111-111111111111", Slug: "escape", Operations: []string{"passthrough"},
		OverallTimeoutMS: 1000, MaxAttempts: 1, Fidelity: fidelity, Behavior: encoded,
		Targets: []routing.PublishedTarget{{ID: "22222222-2222-2222-2222-222222222222", ProviderModelID: "33333333-3333-3333-3333-333333333333", ProviderID: "44444444-4444-4444-4444-444444444444", ProviderName: "p", ProviderModel: "m", Weight: 1, TimeoutMS: 1000}},
	}
}

func validPassthroughBehavior() routing.Behavior {
	return routing.Behavior{Passthrough: &routing.PassthroughConfig{Dialect: "bound-native", Methods: []string{"GET"}, Paths: []string{"/v1/*"}, AcknowledgeOpaqueUnmetered: true}}
}

func TestCheckPassthroughUncertifiedBypass(t *testing.T) {
	d := passthroughDraft(t, validPassthroughBehavior())
	live := map[string]*resolved{"33333333-3333-3333-3333-333333333333": {ProviderID: "44444444-4444-4444-4444-444444444444", ProviderName: "p", ProviderModel: "m", ProviderState: "active", Published: true, Certified: map[string]bool{}}}
	if err := check(d, live); err != nil {
		t.Fatalf("uncertified passthrough refused at publication check: %v", err)
	}
}

func TestCheckPassthroughConflictsRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		behavior routing.Behavior
		attempts int
	}{
		{"fallback", func() routing.Behavior {
			b := validPassthroughBehavior()
			b.Fallbacks = []routing.Fallback{{Route: "other", On: []string{"exhausted"}}}
			return b
		}(), 1},
		{"missing ack", func() routing.Behavior {
			b := validPassthroughBehavior()
			b.Passthrough.AcknowledgeOpaqueUnmetered = false
			return b
		}(), 1},
		{"attempts", validPassthroughBehavior(), 2},
	} {
		d := passthroughDraft(t, tc.behavior)
		d.MaxAttempts = tc.attempts
		live := map[string]*resolved{"33333333-3333-3333-3333-333333333333": {ProviderState: "active", Published: true, Certified: map[string]bool{}}}
		if err := check(d, live); err == nil {
			t.Fatalf("%s accepted at publication check", tc.name)
		}
	}
}

func TestCheckNonPassthroughStillRequiresCertification(t *testing.T) {
	d := passthroughDraft(t, routing.Behavior{})
	d.Operations = []string{"generation"}
	live := map[string]*resolved{"33333333-3333-3333-3333-333333333333": {ProviderState: "active", Published: true, Certified: map[string]bool{}}}
	if err := check(d, live); err == nil {
		t.Fatal("uncertified generation target accepted")
	}
}
