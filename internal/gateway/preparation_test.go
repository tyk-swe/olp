package gateway

import (
	"bytes"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/usage"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestPreparedAdmissionAndNativeRequestSurviveEviction(t *testing.T) {
	for _, strict := range []bool{false, true} {
		release, parsed, providers := preparationFixture(t, strict, 8, 4096)
		route := release.Snapshot.Routes[routeSlug]
		x := &call{request: request{release: release}, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed}}
		var bodies [][]byte
		var estimates []admittedEstimate
		for i := range providers {
			prepared, err := x.preparedProvider(&providers[i], route.Targets[i].ID, "model")
			if err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, prepared.invocation.Prepared.Document().Bytes())
			estimates = append(estimates, prepared.admitted)
			x.attempts = append(x.attempts, routing.Attempt{TargetID: route.Targets[i].ID, ProviderID: providers[i].ID, ProviderRevisionID: providers[i].RevisionID, UpstreamModel: "model"})
		}
		x.finishPreparationPlanning()
		// Include targets beyond the retained first attempts, and revisit a
		// previous target after another attempt took ownership of dispatch.
		for _, i := range []int{7, 0, 5, 0} {
			x.activatePreparation(&providers[i], route.Targets[i].ID, "model")
			if got, err := x.attemptEstimate(&providers[i], route.Targets[i].ID, "model"); err != nil || got != estimates[i] {
				t.Fatalf("admission changed after eviction: %+v %v", got, err)
			}
			prepared, err := x.preparedProvider(&providers[i], route.Targets[i].ID, "model")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(prepared.invocation.Prepared.Document().Bytes(), bodies[i]) {
				t.Fatal("rebuilt native request changed")
			}
		}
	}
}

func TestEffectiveSourceChangeReplacesAllAdmissionState(t *testing.T) {
	release, parsed, providers := preparationFixture(t, false, 1, 128)
	route := release.Snapshot.Routes[routeSlug]
	x := &call{request: request{release: release}, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed}}
	first, err := x.attemptEstimate(&providers[0], route.Targets[0].ID, "model")
	if err != nil {
		t.Fatal(err)
	}
	x.generation.parsed, err = openai.Parse(wire.Chat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"changed"}],"stream":true,"max_tokens":2048}`))
	if err != nil {
		t.Fatal(err)
	}
	next, err := x.attemptEstimate(&providers[0], route.Targets[0].ID, "model")
	if err != nil {
		t.Fatal(err)
	}
	if next.reply == first.reply || next.reply != 2048 {
		t.Fatalf("old source admission survived rewrite: before=%+v after=%+v", first, next)
	}
	prepared, err := x.preparedProvider(&providers[0], route.Targets[0].ID, "model")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(prepared.invocation.Prepared.Document().Bytes(), []byte(`"max_tokens":2048`)) {
		t.Fatal("dispatch rebuilt an earlier source")
	}
}

func TestPreparationUsesExactTargetAndRevision(t *testing.T) {
	release, parsed, providers := preparationFixture(t, true, 1, 128)
	route := release.Snapshot.Routes[routeSlug]
	first := route.Targets[0]
	second := first
	second.ID = "second-target"
	route.Targets = append(route.Targets, second)
	x := &call{request: request{release: release}, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed}}
	if _, err := x.preparedProvider(&providers[0], first.ID, "model"); err != nil {
		t.Fatal(err)
	}
	// The second target has no compiled contract, even though provider and model match.
	if _, err := x.preparedProvider(&providers[0], second.ID, "model"); err == nil {
		t.Fatal("missing target borrowed a sibling's contract")
	}
	if len(x.targets) != 2 {
		t.Fatal("duplicate provider/model targets shared preparation")
	}
}

func TestRouteTransitionDropsPreparationAndLegState(t *testing.T) {
	release, parsed, providers := preparationFixture(t, false, 1, 128)
	first := release.Snapshot.Routes[routeSlug]
	x := &call{request: request{release: release}, routeLeg: routeLeg{route: &first, budget: 4, costExcluded: true}, generation: generationCall{parsed: parsed}}
	if _, err := x.preparedProvider(&providers[0], first.Targets[0].ID, "model"); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Slug = "fallback"
	second.ContentPolicy = nil
	x.useRoute(&second, usage.ViaFallback)
	if len(x.targets) != 0 || x.sourceSummary != nil || x.budget != 0 || x.costExcluded {
		t.Fatal("route transition retained previous leg state")
	}
	if x.generation.parsed != parsed || x.named() != &second {
		t.Fatal("route transition lost call source")
	}
}

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

func TestCostReservationUsesTheSelectedTargetPreparation(t *testing.T) {
	release, parsed, providers := preparationFixture(t, true, 1, 128)
	route := release.Snapshot.Routes[routeSlug]
	target := route.Targets[0]
	provider := providers[0]
	x := &call{request: request{release: release}, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed}}
	if _, err := x.preparedProvider(&provider, target.ID, target.ProviderModel); err != nil {
		t.Fatal(err)
	}
	price := costPrice()
	expected, ok := x.targetCostBound(provider, target, price, nil)
	if !ok {
		t.Fatal("target cost could not be bounded")
	}
	actual := x.attemptCostBound(routing.Attempt{TargetID: target.ID, ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, UpstreamModel: target.ProviderModel, Price: price})
	if actual.Cmp(expected) != 0 || len(x.targets) != 1 {
		t.Fatalf("reservation lost selected target: got %s want %s, %d preparations", actual.String(), expected.String(), len(x.targets))
	}
}
