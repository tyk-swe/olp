package routing

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
)

func TestValidationDoesNotReplaceCompiledContracts(t *testing.T) {
	provider := Provider{ID: uuid.NewString(), Name: "provider", RevisionID: uuid.NewString(), Enabled: true, Kind: "openai_compatible", ProfileID: "compatible-chat", ProfileRevision: connectors.ProfileRevision, AuthMode: "none", Endpoint: "https://upstream.example/v1"}
	target := Target{ID: uuid.NewString(), RoutingID: uuid.NewString(), ProviderID: provider.ID, ProviderModel: "model", Weight: 1, Timeout: 1000}
	route := Route{ID: uuid.NewString(), Slug: "route", RoutingID: uuid.NewString(), Operations: []string{"generation"}, OverallTimeout: 1000, MaxAttempts: 1, Targets: []Target{target}, Fidelity: RouteFidelity{Mode: FidelityStrict}}
	snapshot := &Snapshot{Generation: Generation{ID: uuid.NewString(), Ordinal: 1}, Providers: map[string]Provider{provider.ID: provider}, Routes: map[string]Route{route.Slug: route}}
	if err := snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	before, ok := snapshot.InteractionTemplate(route.Slug, target.ID)
	if !ok {
		t.Fatal("missing compiled target")
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	after, _ := snapshot.InteractionTemplate(route.Slug, target.ID)
	if before != after {
		t.Fatal("validation replaced cached contract")
	}
	provider.ProfileID = "missing-profile"
	snapshot.Providers[provider.ID] = provider
	if err := snapshot.Compile(); err == nil {
		t.Fatal("invalid profile compiled")
	}
	after, _ = snapshot.InteractionTemplate(route.Slug, target.ID)
	if before != after {
		t.Fatal("failed compilation replaced cached contract")
	}
}

func TestSnapshotCloneOwnsPoliciesTargetsAndCodeConnections(t *testing.T) {
	route := Route{
		Targets:       []Target{{Tags: []string{"original"}}},
		ContentPolicy: &contentpolicy.Policy{Rules: []contentpolicy.Rule{{Pattern: "original"}}},
		Policy:        &Policy{Defaults: Preferences{Only: []string{"original"}, MaxAttempts: new(2)}},
		Behavior:      Behavior{Selectors: []Selector{{When: Predicate{Classifier: &ClassifierPredicate{Labels: []string{"original"}, MinScore: new(0.5)}}}}},
	}
	source := &Snapshot{
		Routes:          map[string]Route{"route": route},
		CodeRoutes:      map[string]codemode.Route{"code": {Models: []string{"original"}, MaxBodyBytes: new(int64(100))}},
		CodeConnections: map[string]Configuration{"connection": {}},
	}
	connection := source.CodeConnections["connection"]
	connection.Options.Models = map[string]json.RawMessage{"model": json.RawMessage(`{"context_length":100}`)}
	source.CodeConnections["connection"] = connection
	clone := source.Clone()
	changed := clone.Routes["route"]
	changed.Targets[0].Tags[0] = "changed"
	changed.ContentPolicy.Rules[0].Pattern = "changed"
	changed.Policy.Defaults.Only[0] = "changed"
	*changed.Policy.Defaults.MaxAttempts = 9
	changed.Selectors[0].When.Classifier.Labels[0] = "changed"
	*changed.Selectors[0].When.Classifier.MinScore = 1
	code := clone.CodeRoutes["code"]
	code.Models[0] = "changed"
	*code.MaxBodyBytes = 200
	clone.CodeConnections["connection"].Options.Models["model"][0] = '['
	original := source.Routes["route"]
	if original.Targets[0].Tags[0] != "original" || original.ContentPolicy.Rules[0].Pattern != "original" || original.Policy.Defaults.Only[0] != "original" || *original.Policy.Defaults.MaxAttempts != 2 || original.Selectors[0].When.Classifier.Labels[0] != "original" || *original.Selectors[0].When.Classifier.MinScore != 0.5 || source.CodeRoutes["code"].Models[0] != "original" || *source.CodeRoutes["code"].MaxBodyBytes != 100 || source.CodeConnections["connection"].Options.Models["model"][0] != '{' {
		t.Fatal("snapshot clone shared mutable configuration")
	}
}
