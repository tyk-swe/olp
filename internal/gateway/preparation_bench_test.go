package gateway

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	serving "github.com/tyk-swe/olp/internal/runtime"
)

// Keep the measurement's requests live across GC, as a slow stream does. This
// measures reachable memory, independently of total allocation during planning.
var preparationMemory []*call

func preparationFixture(tb testing.TB, strict bool, count, bytes int) (*serving.Release, *wire.Request, []routing.Provider) {
	tb.Helper()
	providers := make([]routing.Provider, count)
	route := routing.Route{RoutingID: uuid.NewString(), ID: uuid.NewString(), Slug: routeSlug, RevisionID: uuid.NewString(), Revision: 1, PublishedAt: time.Now(), OverallTimeout: 10000, MaxAttempts: count, Operations: []string{"generation"}, Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}}
	snapshot := &routing.Snapshot{Generation: routing.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()}, Providers: map[string]routing.Provider{}, Routes: map[string]routing.Route{}}
	if strict {
		route.Fidelity.Mode = routing.FidelityStrict
	}
	for i := range providers {
		providers[i] = routing.Provider{ID: uuid.NewString(), RevisionID: uuid.NewString(), Name: fmt.Sprint(i), Enabled: true, Kind: "openai_compatible", AuthMode: "none", Endpoint: "https://upstream.example/v1", ProfileID: "compatible-chat", ProfileRevision: connectors.ProfileRevision, Capabilities: []routing.Capability{{Model: "model", Operation: "generation", Surface: "openai", Mode: "streaming"}}, Slots: []routing.Slot{{ID: uuid.NewString(), Enabled: true, Weight: 1}}}
		snapshot.Providers[providers[i].ID] = providers[i]
		route.Targets = append(route.Targets, routing.Target{RoutingID: uuid.NewString(), ID: uuid.NewString(), ProviderID: providers[i].ID, ProviderModel: "model", Weight: 1, Timeout: 9000})
	}
	snapshot.Routes[routeSlug] = route
	release, err := serving.NewRelease(snapshot.Generation.ID, 1, snapshot, nil)
	if err != nil {
		tb.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("a", bytes)}}, "stream": true, "max_tokens": 16})
	parsed, err := openai.Parse(wire.Chat, body)
	if err != nil {
		tb.Fatal(err)
	}
	return release, parsed, providers
}

func BenchmarkPreparedPlanning(b *testing.B) {
	for _, strict := range []bool{false, true} {
		for _, count := range []int{1, 4, 16} {
			b.Run(fmt.Sprintf("strict=%t/targets=%d", strict, count), func(b *testing.B) {
				release, parsed, providers := preparationFixture(b, strict, count, 64<<10)
				route := release.Snapshot.Routes[routeSlug]
				b.ReportAllocs()
				for b.Loop() {
					x := &call{request: request{release: release}, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed}}
					for i := range providers {
						if _, err := x.preparedProvider(&providers[i], route.Targets[i].ID, "model"); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

func BenchmarkPreparedRetention(b *testing.B) {
	for _, strict := range []bool{false, true} {
		for _, count := range []int{1, 4, 16} {
			b.Run(fmt.Sprintf("strict=%t/targets=%d", strict, count), func(b *testing.B) {
				release, parsed, providers := preparationFixture(b, strict, count, 1<<20)
				route := release.Snapshot.Routes[routeSlug]
				preparationMemory = nil
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				for range 8 {
					x := &call{request: request{release: release}, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed}}
					for i := range providers {
						if _, err := x.preparedProvider(&providers[i], route.Targets[i].ID, "model"); err != nil {
							b.Fatal(err)
						}
					}

					for i, p := range providers {
						x.attempts = append(x.attempts, routing.Attempt{TargetID: route.Targets[i].ID, ProviderID: p.ID, ProviderRevisionID: p.RevisionID, UpstreamModel: "model"})
					}
					x.finishPreparationPlanning()
					x.activatePreparation(&providers[0], route.Targets[0].ID, "model")
					preparationMemory = append(preparationMemory, x)
				}
				runtime.GC()
				runtime.ReadMemStats(&after)
				b.ReportMetric(float64(max(int64(after.HeapAlloc)-int64(before.HeapAlloc), 0)/8), "retained-B/request")
				preparationMemory = nil
			})
		}
	}
}
