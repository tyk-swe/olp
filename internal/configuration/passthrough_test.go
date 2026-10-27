package configuration

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func passthroughEntry() *RouteEntry {
	return &RouteEntry{
		Slug: "escape", Operations: []string{"passthrough"}, OverallTimeoutMS: 1000, MaxAttempts: 1,
		Fidelity:    json.RawMessage(`{"mode":"strict"}`),
		Targets:     []TargetEntry{{Provider: "p", ProviderModel: "m", Weight: 1, TimeoutMS: 1000}},
		Passthrough: &routing.PassthroughConfig{Dialect: "bound-native", Methods: []string{"GET"}, Paths: []string{"/v1/*"}, AcknowledgeOpaqueUnmetered: true},
	}
}

func TestRouteEntryPassthroughPlanValidation(t *testing.T) {
	if err := passthroughEntry().validatePassthrough(); err != nil {
		t.Fatalf("valid entry refused at plan: %v", err)
	}
	for _, mutate := range []func(*RouteEntry){
		func(r *RouteEntry) { r.Fallbacks = []routing.Fallback{{Route: "other", On: []string{"exhausted"}}} },
		func(r *RouteEntry) { r.Targets = append(r.Targets, r.Targets[0]) },
		func(r *RouteEntry) { r.Passthrough.AcknowledgeOpaqueUnmetered = false },
		func(r *RouteEntry) {
			r.ContentPolicy = json.RawMessage(`{"rules":[{"id":"x","pattern":"a","action":"block"}]}`)
		},
	} {
		r := passthroughEntry()
		mutate(r)
		if err := r.validatePassthrough(); err == nil {
			t.Fatalf("conflicting entry accepted at plan: %+v", r)
		}
	}
}

func TestRouteEntryPassthroughBehaviorRoundTrip(t *testing.T) {
	raw, err := json.Marshal(passthroughEntry().behavior())
	if err != nil {
		t.Fatal(err)
	}
	var out RouteEntry
	if err := out.setBehavior(raw); err != nil {
		t.Fatal(err)
	}
	if out.Passthrough == nil || out.Passthrough.Dialect != "bound-native" || !out.Passthrough.AcknowledgeOpaqueUnmetered {
		t.Fatalf("passthrough lost in round trip: %+v", out.Passthrough)
	}
}
