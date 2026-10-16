package gateway

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

// Include targets beyond the retained first attempts, and revisit a
// previous target after another attempt took ownership of dispatch.

// The second target has no compiled contract, even though provider and model match.

func TestEvictedPreparationRetainsRequiredParameterMetadata(t *testing.T) {
	release, parsed, providers := preparationFixture(t, true, 8, 128)
	route := release.Snapshot.Routes[routeSlug]
	route.Policy = &routing.Policy{Constraints: routing.Preferences{RequireParameters: new(true)}}
	x := &call{request: request{release: release}, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed}}
	for i := range providers {
		if _, err := x.preparedProvider(&providers[i], route.Targets[i].ID, "model"); err != nil {
			t.Fatal(err)
		}
	}
	record := x.targetPreparation(&providers[7], route.Targets[7].ID, "model")
	if record.heavy() {
		t.Fatal("evicted target retained its body")
	}
	if record.provider.parameters == nil || !slices.Contains(record.provider.parameters(), "max_output_tokens") {
		t.Fatal("parameter policy lost its admission metadata")
	}
}
