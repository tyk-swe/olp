package routing

import "testing"

func TestRetainedAgentRoutePublication(t *testing.T) {
	route := Route{Slug: "agents", Operations: []string{"agent_resource"}, Targets: []Target{{ID: "target", ProviderID: "provider", ProviderModel: "gemini-model"}}}
	snapshot := Snapshot{Providers: map[string]Provider{"provider": {Kind: "gemini", ProfileID: "gemini-agents", ProfileRevision: "1", Endpoint: "https://generativelanguage.googleapis.com/v1beta", AuthMode: "api_key"}}}
	if templates, err := snapshot.compileOperations(route); err != nil || len(templates) != 0 {
		t.Fatalf("retained route incorrectly compiled as unary: %v %v", templates, err)
	}
	route.Fidelity = RouteFidelity{Mode: FidelityTransformed}
	if _, err := snapshot.compileOperations(route); err == nil {
		t.Fatal("native resource published on transformed route")
	}
	route.Fidelity = RouteFidelity{Mode: FidelityStrict}
	provider := snapshot.Providers["provider"]
	provider.ProfileID = "gemini-generation"
	snapshot.Providers["provider"] = provider
	if _, err := snapshot.compileOperations(route); err == nil {
		t.Fatal("unqualified provider published")
	}
}
