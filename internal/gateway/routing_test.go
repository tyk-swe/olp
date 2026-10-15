package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestRoutingUsesCurrentMeasurements(t *testing.T) {
	for _, strategy := range []string{"price", "latency"} {
		for _, state := range []string{"missing", "fresh", "expired"} {
			t.Run(strategy+"/"+state, func(t *testing.T) {
				h := newHarness(t, Config{})
				now := time.Now()
				h.gateway.now = func() time.Time { return now }
				snapshot := h.rt.release.Snapshot
				route := snapshot.Routes[routeSlug]
				for i := range route.Targets {
					route.Targets[i].Priority = 0
				}
				snapshot.Routes[routeSlug] = route
				weighted, err := routing.Select(snapshot, routeSlug, "generation", "openai", "unary", []byte(h.keyID), routing.Options{})
				if err != nil || len(weighted.Attempts) != 2 {
					t.Fatalf("weighted candidates=%v error=%v", weighted, err)
				}
				// Give only the second weighted candidate measurements, so the
				// strategy must change the selection when those inputs are fresh.
				measured := weighted.Attempts[1]
				price := "1"
				if state != "missing" {
					inputs := &usage.RoutingInputs{
						RefreshedAt: now,
						Prices: []usage.RoutingPrice{{
							Price:       usage.Price{ProviderKind: measured.ProviderKind, ProviderID: &measured.ProviderID, Model: measured.UpstreamModel, Operation: "generation", InputPerMillion: &price, OutputPerMillion: &price, Currency: "USD"},
							EffectiveAt: now.Add(-time.Hour),
						}},
						Performance: map[string]usage.Performance{
							usage.PerformanceKey(measured.ProviderID, measured.UpstreamModel, "generation", "unary"): {SampleCount: 20, LatencyMS: 1, ObservedAt: now},
						},
					}
					if state == "expired" {
						inputs.RefreshedAt = now.Add(-usage.PerformanceMaxAge - time.Second)
					}
					h.rt.inputs = inputs
				}
				want := weighted.Attempts[0].ProviderID
				if state == "fresh" {
					want = measured.ProviderID
				}
				response, body := h.chat(fullKey, map[string]string{routingHeader: `{"strategy":"` + strategy + `"}`})
				if response.StatusCode != http.StatusOK {
					t.Fatalf("status=%d body=%v", response.StatusCode, body)
				}
				for id, provider := range snapshot.Providers {
					count := 0
					if id == want {
						count = 1
					}
					if got := h.mock.count(provider.Name); got != count {
						t.Errorf("provider %s calls=%d, want %d", provider.Name, got, count)
					}
				}
			})
		}
	}
}

func TestSimulationAndExecutionAgreeForDuplicateProviderModelTargets(t *testing.T) {
	h := newHarness(t, Config{})
	snapshot := h.rt.release.Snapshot
	route := snapshot.Routes[routeSlug]
	first := route.Targets[0]
	first.Priority = 1
	second := first
	second.ID = uuid.NewString()
	second.RoutingID = uuid.NewString()
	second.Priority = 0
	route.Targets = []routing.Target{first, second}
	snapshot.Routes[routeSlug] = route
	plan, err := routing.Select(snapshot, routeSlug, "generation", "openai", "unary", []byte(h.keyID), routing.Options{KeyID: h.keyID, CheckSlots: true, CredentialEligibility: h.rt.Eligibility})
	if err != nil || len(plan.Attempts) == 0 {
		t.Fatal("simulation failed", err)
	}
	response, body := h.chat(fullKey, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("execution failed: %d %v", response.StatusCode, body)
	}
	facts := h.sink.last(t).Attempts
	if len(facts) != 1 || facts[0].TargetID != plan.Attempts[0].TargetID || facts[0].TargetID != second.ID {
		t.Fatalf("execution %+v disagrees with simulation %+v", facts, plan.Attempts)
	}
}
