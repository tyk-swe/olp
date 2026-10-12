package gateway

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	serving "github.com/tyk-swe/olp/internal/runtime"
)

// Keep the measurement's requests live across GC, as a slow stream does. This
// measures reachable memory, independently of total allocation during planning.
var preparationMemory []*execution

func preparationFixture(tb testing.TB, strict bool, count, bytes int) (*serving.Release, *openai.Request, []serving.Provider) {
	tb.Helper()
	providers := make([]serving.Provider, count)
	route := serving.Route{RoutingID: uuid.NewString(), ID: uuid.NewString(), Slug: routeSlug, RevisionID: uuid.NewString(), Revision: 1, PublishedAt: time.Now(), OverallTimeout: 10000, MaxAttempts: count, Operations: []string{"generation"}, Fidelity: serving.RouteFidelity{Mode: serving.FidelityTransformed}}
	snapshot := &serving.Snapshot{Generation: serving.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()}, Providers: map[string]serving.Provider{}, Routes: map[string]serving.Route{}}
	if strict {
		route.Fidelity.Mode = serving.FidelityStrict
	}
	for i := range providers {
		providers[i] = serving.Provider{ID: uuid.NewString(), RevisionID: uuid.NewString(), Name: fmt.Sprint(i), Enabled: true, Kind: "openai_compatible", AuthMode: "none", Endpoint: "https://upstream.example/v1", ProfileID: "compatible-chat", ProfileRevision: connectors.ProfileRevision, Capabilities: []serving.Capability{{Model: "model", Operation: "generation", Surface: "openai", Mode: "streaming"}}, Slots: []serving.Slot{{ID: uuid.NewString(), Enabled: true, Weight: 1}}}
		snapshot.Providers[providers[i].ID] = providers[i]
		route.Targets = append(route.Targets, serving.Target{RoutingID: uuid.NewString(), ID: uuid.NewString(), ProviderID: providers[i].ID, ProviderModel: "model", Weight: 1, Timeout: 9000})
	}
	snapshot.Routes[routeSlug] = route
	release, err := serving.NewRelease(snapshot.Generation.ID, 1, snapshot, nil)
	if err != nil {
		tb.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("a", bytes)}}, "stream": true, "max_tokens": 16})
	parsed, err := openai.Parse(openai.FamilyChat, body)
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
					x := &execution{request: request{release: release}, parsed: parsed, route: &route}
					for i := range providers {
						if _, err := x.preparedProvider(&providers[i], "model"); err != nil {
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
					x := &execution{request: request{release: release}, parsed: parsed, route: &route}
					for i := range providers {
						if _, err := x.preparedProvider(&providers[i], "model"); err != nil {
							b.Fatal(err)
						}
					}

					for _, p := range providers {
						x.attempts = append(x.attempts, serving.Attempt{ProviderID: p.ID, ProviderRevisionID: p.RevisionID, UpstreamModel: "model"})
					}
					x.finishPreparationPlanning()
					x.activatePreparation(&providers[0], "model")
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
