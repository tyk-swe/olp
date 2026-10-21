package modelcatalog

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/usage"
)

func pointer[T any](value T) *T { return &value }

func catalogFixture(t *testing.T) (*routing.Snapshot, routing.Route, *usage.RoutingInputs, time.Time) {
	t.Helper()
	now := time.Now()
	snapshot := &routing.Snapshot{Providers: map[string]routing.Provider{}}
	route := routing.Route{Slug: "assistant", Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed}, Operations: []string{"generation"}, Targets: []routing.Target{}}
	inputs := &usage.RoutingInputs{RefreshedAt: now}
	for index, id := range []string{"first", "second"} {
		name := "private-upstream-" + id
		facts := routing.ModelMetadata{ContextLength: pointer(int64(32000 + index*32000)), InputModalities: []string{"text"}, OutputModalities: []string{"text"}, DataCollection: pointer(false), ZeroDataRetention: pointer(true), Region: pointer(id)}
		raw, err := json.Marshal(facts)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Providers[id] = routing.Provider{ID: id, Name: "private-provider-" + id, Kind: "openai_compatible", Enabled: true, Endpoint: "https://private-provider.example", Capabilities: []routing.Capability{{Model: name, Operation: "generation", Surface: "openai", Mode: "unary"}, {Model: name, Operation: "generation", Surface: "anthropic", Mode: "unary"}, {Model: name, Operation: "generation", Surface: "gemini", Mode: "unary"}}, Models: map[string]json.RawMessage{name: raw}}
		route.Targets = append(route.Targets, routing.Target{ProviderID: id, ProviderModel: name})
		input := "1.200000000000000001"
		if index == 1 {
			input = "4.000000000000000001"
		}
		inputs.Prices = append(inputs.Prices, usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai_compatible", ProviderID: pointer(id), Model: name, Operation: "generation", InputPerMillion: pointer(input), OutputPerMillion: pointer("5"), Currency: "USD"}, EffectiveAt: now.Add(-time.Hour)})
	}
	return snapshot, route, inputs, now
}

func TestCatalogKeepsUnknownPrivacyAndPartialPricesExplicit(t *testing.T) {
	snapshot, route, inputs, now := catalogFixture(t)
	provider := snapshot.Providers["second"]
	provider.Models[route.Targets[1].ProviderModel] = json.RawMessage(`{"data_collection":true,"zero_data_retention":false}`)
	model := Describe(snapshot, route, inputs, "https://gateway.example", true, false, now)
	if model.Privacy.DataCollection == nil || !*model.Privacy.DataCollection || model.Privacy.ZeroDataRetention == nil || *model.Privacy.ZeroDataRetention {
		t.Fatal("an adverse declaration was hidden")
	}
	provider.Models[route.Targets[1].ProviderModel] = json.RawMessage(`{}`)
	inputs.Prices = inputs.Prices[:1]
	model = Describe(snapshot, route, inputs, "https://gateway.example", true, false, now)
	if model.Privacy.DataCollection != nil || model.Privacy.ZeroDataRetention != nil || len(model.Privacy.Unknown) != 3 {
		t.Fatalf("unknown facts asserted: %+v", model.Privacy)
	}
	if len(model.Prices) != 1 || model.Prices[0].Complete {
		t.Fatal("partially priced targets asserted complete prices")
	}
}
