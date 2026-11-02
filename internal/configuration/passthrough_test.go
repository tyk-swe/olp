package configuration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestSinkNormalizationPreservesSourceStreams(t *testing.T) {
	source := []string{"requests", "attempts"}
	doc := Document{Sinks: []SinkEntry{{Name: "sink", Type: "https", Destination: "https://example.test/export", Streams: source}}}
	server := &Server{Egress: &egress.Policy{}}
	if err := server.validateAndNormalizeSinks(&doc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source, []string{"requests", "attempts"}) {
		t.Fatalf("normalization changed the source streams: %v", source)
	}
	if !reflect.DeepEqual(doc.Sinks[0].Streams, []string{"attempts", "requests"}) {
		t.Fatalf("streams were not normalized: %v", doc.Sinks[0].Streams)
	}
}

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

func TestPlanComparisonsPreserveInputs(t *testing.T) {
	t.Run("provider", func(t *testing.T) {
		entry := func() ProviderEntry {
			return ProviderEntry{
				Name: "provider",
				Models: []ModelEntry{
					{UpstreamModel: "second", Capabilities: []CapabilityEntry{{Operation: "embeddings"}, {Operation: "generation"}}},
					{UpstreamModel: "first"},
				},
				Slots: []SlotEntry{
					{Name: "second", Position: 2, Restrictions: Restrictions{
						AllowedAPIKeys: []string{"first", "second"}, AllowedModels: []string{"first", "second"}, AllowedRoutes: []string{"first", "second"},
					}},
					{Name: "first", Position: 1},
				},
			}
		}
		other := func() ProviderEntry {
			return ProviderEntry{
				Name: "provider",
				Models: []ModelEntry{
					{UpstreamModel: "first"},
					{UpstreamModel: "second", Capabilities: []CapabilityEntry{{Operation: "generation"}, {Operation: "embeddings"}}},
				},
				Slots: []SlotEntry{
					{Name: "first", Position: 1},
					{Name: "second", Position: 2, Restrictions: Restrictions{
						AllowedAPIKeys: []string{"second", "first"}, AllowedModels: []string{"second", "first"}, AllowedRoutes: []string{"second", "first"},
					}},
				},
			}
		}
		left, right := entry(), other()
		if !canonicalEqualProvider(&left, &right) {
			t.Fatal("matching providers compared unequal")
		}
		if !reflect.DeepEqual(left, entry()) || !reflect.DeepEqual(right, other()) {
			t.Fatal("provider comparison changed its inputs")
		}
	})

	t.Run("route", func(t *testing.T) {
		entry := func() RouteEntry {
			return RouteEntry{
				Slug: "route", Operations: []string{"generation", "embeddings"},
				Targets: []TargetEntry{{Provider: "second", Tags: []string{"second", "first"}}, {Provider: "first"}},
			}
		}
		other := func() RouteEntry {
			return RouteEntry{
				Slug: "route", Operations: []string{"embeddings", "generation"},
				Targets: []TargetEntry{{Provider: "first"}, {Provider: "second", Tags: []string{"first", "second"}}},
			}
		}
		left, right := entry(), other()
		if !canonicalEqualRoute(&left, &right) {
			t.Fatal("matching routes compared unequal")
		}
		if !reflect.DeepEqual(left, entry()) || !reflect.DeepEqual(right, other()) {
			t.Fatal("route comparison changed its inputs")
		}
	})
}
