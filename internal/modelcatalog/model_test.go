package modelcatalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestSamplesMatchAvailableNativeSDKOperations(t *testing.T) {
	route := runtime.Route{Slug: "embed", Operations: []string{"embeddings", "token_count"}}
	got := samples(route, "https://gateway.example.com")
	if len(got) != 4 {
		t.Fatalf("native operation samples: %v", got)
	}
	for _, sample := range got {
		if sample.Operation == "generation" || sample.SDK == "anthropic" && sample.Operation == "embeddings" {
			t.Fatalf("unsupported SDK interface: %v", sample)
		}
		if !strings.Contains(sample.Code, "OLP_API_KEY") || !strings.Contains(sample.Code, `model="embed"`) {
			t.Fatal("sample bypasses route or mounted credential convention")
		}
	}
}

func pointer[T any](value T) *T { return &value }

func catalogFixture(t *testing.T) (*runtime.Snapshot, runtime.Route, *usage.RoutingInputs, time.Time) {
	t.Helper()
	now := time.Now()
	snapshot := &runtime.Snapshot{Providers: map[string]runtime.Provider{}}
	route := runtime.Route{Slug: "assistant", Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityTransformed}, Operations: []string{"generation"}, Targets: []runtime.Target{}}
	inputs := &usage.RoutingInputs{RefreshedAt: now}
	for index, id := range []string{"first", "second"} {
		name := "private-upstream-" + id
		facts := runtime.ModelMetadata{ContextLength: pointer(int64(32000 + index*32000)), InputModalities: []string{"text"}, OutputModalities: []string{"text"}, DataCollection: pointer(false), ZeroDataRetention: pointer(true), Region: pointer(id)}
		raw, err := json.Marshal(facts)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Providers[id] = runtime.Provider{ID: id, Name: "private-provider-" + id, Kind: "openai_compatible", Enabled: true, Endpoint: "https://private-provider.example", Capabilities: []runtime.Capability{{Model: name, Operation: "generation", Surface: "openai", Mode: "unary"}, {Model: name, Operation: "generation", Surface: "anthropic", Mode: "unary"}, {Model: name, Operation: "generation", Surface: "gemini", Mode: "unary"}}, Models: map[string]json.RawMessage{name: raw}}
		route.Targets = append(route.Targets, runtime.Target{ProviderID: id, ProviderModel: name})
		input := "1.200000000000000001"
		if index == 1 {
			input = "4.000000000000000001"
		}
		inputs.Prices = append(inputs.Prices, usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai_compatible", ProviderID: pointer(id), Model: name, Operation: "generation", InputPerMillion: pointer(input), OutputPerMillion: pointer("5"), Currency: "USD"}, EffectiveAt: now.Add(-time.Hour)})
	}
	return snapshot, route, inputs, now
}

func TestCatalogProjectsConservativeFactsAndPricesWithoutProviderIdentities(t *testing.T) {
	snapshot, route, inputs, now := catalogFixture(t)
	model := Describe(snapshot, route, inputs, "https://gateway.example", true, false, now)
	if model.Capabilities.ContextLength == nil || *model.Capabilities.ContextLength != 32000 {
		t.Fatal("context is not the common bound")
	}
	if model.Privacy.ZeroDataRetention == nil || !*model.Privacy.ZeroDataRetention || model.Privacy.DataCollection == nil || *model.Privacy.DataCollection {
		t.Fatalf("privacy: %+v", model.Privacy)
	}
	if len(model.Prices) != 1 || !model.Prices[0].Complete || model.Prices[0].InputPerMillion.Minimum != "1.200000000000000001" || model.Prices[0].InputPerMillion.Maximum != "4.000000000000000001" {
		t.Fatalf("prices lost precision or coverage: %+v", model.Prices)
	}
	if model.Prices[0].CachedInputPerMillion.Maximum != model.Prices[0].InputPerMillion.Maximum {
		t.Fatal("cached price did not inherit input price")
	}
	raw, _ := json.Marshal(model)
	if strings.Contains(string(raw), "private-") {
		t.Fatal("catalog exposed provider/model/endpoint metadata")
	}
	if len(model.Samples) != 3 {
		t.Fatal("missing qualified generation SDK samples")
	}
	for _, sample := range model.Samples {
		if !strings.Contains(sample.Code, "assistant") || !strings.Contains(sample.Code, "OLP_API_KEY") {
			t.Fatalf("invalid sample: %+v", sample)
		}
	}
}

func TestCatalogPublicPriceAndUpstreamChoicesAreIndependent(t *testing.T) {
	snapshot, route, inputs, now := catalogFixture(t)
	model := Describe(snapshot, route, inputs, "https://gateway.example", false, true, now)
	if len(model.Prices) != 0 || len(model.UpstreamModels) != 2 {
		t.Fatalf("independent disclosures: %+v", model)
	}
	model = Describe(snapshot, route, inputs, "https://gateway.example", true, false, now.Add(usage.PerformanceMaxAge+time.Second))
	if len(model.Prices) != 0 {
		t.Fatal("stale price metadata exposed as current")
	}
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

func TestCatalogSamplesRespectCertifiedModesAndStrictNativeDialect(t *testing.T) {
	snapshot, route, inputs, now := catalogFixture(t)
	first := snapshot.Providers["first"]
	first.Capabilities = []runtime.Capability{{Model: route.Targets[0].ProviderModel, Operation: "generation", Surface: "openai", Mode: "unary"}, {Model: route.Targets[0].ProviderModel, Operation: "generation", Surface: "anthropic", Mode: "stream"}}
	second := snapshot.Providers["second"]
	second.Enabled = false
	snapshot.Providers["first"], snapshot.Providers["second"] = first, second
	model := Describe(snapshot, route, inputs, "https://gateway.example", false, false, now)
	if len(model.Samples) != 1 || model.Samples[0].SDK != "openai" {
		t.Fatalf("uncertified examples: %+v", model.Samples)
	}
	route.Fidelity = runtime.RouteFidelity{Mode: runtime.FidelityStrict}
	first.ProfileID = "compatible-responses"
	first.ProfileRevision = connectors.ProfileRevision
	snapshot.Providers["first"] = first
	model = Describe(snapshot, route, inputs, "https://gateway.example", false, false, now)
	if len(model.Samples) != 1 || !strings.Contains(model.Samples[0].Code, "client.responses.create") || strings.Contains(model.Samples[0].Code, "chat.completions") {
		t.Fatalf("strict dialect example: %+v", model.Samples)
	}
	first.Capabilities = nil
	snapshot.Providers["first"] = first
	if got := Describe(snapshot, route, inputs, "https://gateway.example", false, false, now).Samples; len(got) != 0 {
		t.Fatalf("uncertified route examples: %+v", got)
	}
}
