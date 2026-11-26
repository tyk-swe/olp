package providers

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/connectors/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestSameJSONComparesValuesAndPropagatesMarshalErrors(t *testing.T) {
	t.Parallel()

	t.Run("equal maps compare true regardless of insertion order", func(t *testing.T) {
		t.Parallel()
		same, err := sameJSON(map[string]int{"b": 2, "a": 1}, map[string]int{"a": 1, "b": 2})
		if err != nil || !same {
			t.Fatalf("sameJSON = (%v, %v), want (true, nil)", same, err)
		}
	})

	t.Run("nil and empty slices serialize distinctly", func(t *testing.T) {
		t.Parallel()
		var nilSlice []string
		same, err := sameJSON(nilSlice, []string{})
		if err != nil || same {
			t.Fatalf("sameJSON(nil, []) = (%v, %v), want (false, nil)", same, err)
		}
	})

	t.Run("marshal failure on left side returns error", func(t *testing.T) {
		t.Parallel()
		same, err := sameJSON(math.NaN(), 1.0)
		if err == nil || same {
			t.Fatalf("sameJSON(NaN, 1) = (%v, %v), want (false, error)", same, err)
		}
	})

	t.Run("marshal failure on both sides returns error instead of equal", func(t *testing.T) {
		t.Parallel()
		same, err := sameJSON(math.NaN(), math.Inf(1))
		if err == nil || same {
			t.Fatalf("sameJSON(NaN, +Inf) = (%v, %v), want (false, error)", same, err)
		}
	})
}

func TestDiffRevisionsReportsChangesAndPropagatesErrors(t *testing.T) {
	t.Parallel()

	t.Run("identical revisions serialize non-nil empty arrays", func(t *testing.T) {
		t.Parallel()
		rev := &revisionRow{
			Revision: 1,
			Name:     "primary",
			Configuration: connection.Configuration{
				Identity: providerconfig.Identity[*string]{
					Kind:     connectors.KindOpenAI,
					AuthMode: connectors.AuthAPIKey,
					Endpoint: new("https://api.example/v1"),
				},
			},
			Models: []routing.RevisionModel{
				{
					ID:            "model-1",
					UpstreamModel: "gpt-a",
					DisplayName:   "GPT A",
					Capabilities:  []routing.RevisionCapability{{Operation: "generation", Surface: "openai", Mode: "unary", Source: "certified"}},
				},
			},
			CredentialVersion: new(1),
		}
		diff, err := diffRevisions(rev, rev)
		if err != nil {
			t.Fatalf("diffRevisions returned error: %v", err)
		}
		if diff.NameChanged || diff.EndpointChanged || diff.ConnectorChanged || diff.CredentialChanged ||
			diff.ProfileChanged || diff.PluginChanged || diff.PluginOptionsChanged ||
			diff.SemanticConfigurationChanged || diff.ServingBindingChanged || diff.NetworkConfigurationChanged ||
			diff.CloudContextChanged || diff.DeploymentChanged || diff.APIVersionChanged {
			t.Fatalf("diff of identical revisions flagged change: %+v", diff)
		}
		encoded, err := json.Marshal(diff)
		if err != nil {
			t.Fatalf("marshal diff: %v", err)
		}
		var wire map[string]any
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatalf("unmarshal diff: %v", err)
		}
		for _, field := range []string{"models_added", "models_removed", "models_changed", "capabilities_added", "capabilities_removed"} {
			list, ok := wire[field].([]any)
			if !ok || len(list) != 0 {
				t.Fatalf("field %q = %#v, want empty JSON array []", field, wire[field])
			}
		}
	})

	t.Run("model, capability, configuration, and principal changes are classified", func(t *testing.T) {
		t.Parallel()
		from := &revisionRow{
			Revision: 1,
			Name:     "old-name",
			Configuration: connection.Configuration{
				Identity: providerconfig.Identity[*string]{
					Kind:     connectors.KindOpenAI,
					AuthMode: connectors.AuthAPIKey,
					Endpoint: new("https://old.example/v1"),
				},
			},
			Models: []routing.RevisionModel{
				{
					ID:            "m-removed",
					UpstreamModel: "model-removed",
					DisplayName:   "Removed",
					Capabilities:  []routing.RevisionCapability{{Operation: "generation", Surface: "openai", Mode: "unary", Source: "certified"}},
				},
				{
					ID:            "m-changed",
					UpstreamModel: "model-changed",
					DisplayName:   "Before",
					Capabilities:  []routing.RevisionCapability{{Operation: "generation", Surface: "openai", Mode: "unary", Source: "certified"}},
				},
			},
			Slots:             []routing.RevisionSlot{{ObservedPrincipal: "acct-a"}},
			CredentialVersion: new(1),
		}
		to := &revisionRow{
			Revision: 2,
			Name:     "new-name",
			Configuration: connection.Configuration{
				Identity: providerconfig.Identity[*string]{
					Kind:       connectors.KindOpenAI,
					AuthMode:   connectors.AuthAPIKey,
					Endpoint:   new("https://new.example/v1"),
					APIVersion: new("2025-01-01"),
				},
				Options: connection.Options{
					PluginOptions: map[string]string{"tier": "enterprise"},
				},
			},
			Models: []routing.RevisionModel{
				{
					ID:            "m-changed",
					UpstreamModel: "model-changed",
					DisplayName:   "After",
					Capabilities: []routing.RevisionCapability{
						{Operation: "generation", Surface: "openai", Mode: "unary", Source: "certified"},
						{Operation: "generation", Surface: "openai", Mode: "streaming", Source: "certified"},
					},
				},
				{
					ID:            "m-added",
					UpstreamModel: "model-added",
					DisplayName:   "Added",
					Capabilities:  []routing.RevisionCapability{{Operation: "embeddings", Surface: "openai", Mode: "unary", Source: "certified"}},
				},
			},
			Slots:             []routing.RevisionSlot{{ObservedPrincipal: "acct-b"}},
			CredentialVersion: new(2),
		}
		diff, err := diffRevisions(from, to)
		if err != nil {
			t.Fatalf("diffRevisions returned error: %v", err)
		}
		if !diff.NameChanged || !diff.EndpointChanged || !diff.APIVersionChanged ||
			!diff.PluginOptionsChanged || !diff.ServingBindingChanged || !diff.CredentialChanged ||
			diff.ConnectorChanged || diff.ProfileChanged {
			t.Fatalf("unexpected diff flags: %+v", diff)
		}
		if !reflect.DeepEqual(diff.ModelsAdded, []string{"model-added"}) ||
			!reflect.DeepEqual(diff.ModelsRemoved, []string{"model-removed"}) ||
			!reflect.DeepEqual(diff.ModelsChanged, []string{"model-changed"}) {
			t.Fatalf("model diff = added:%v removed:%v changed:%v", diff.ModelsAdded, diff.ModelsRemoved, diff.ModelsChanged)
		}
		if !reflect.DeepEqual(diff.CapabilitiesAdded, []string{"model-added:embeddings/openai/unary", "model-changed:generation/openai/streaming"}) ||
			!reflect.DeepEqual(diff.CapabilitiesRemoved, []string{"model-removed:generation/openai/unary"}) {
			t.Fatalf("capability diff = added:%v removed:%v", diff.CapabilitiesAdded, diff.CapabilitiesRemoved)
		}
	})

	t.Run("malformed operation default JSON returns an error", func(t *testing.T) {
		t.Parallel()
		bad := &revisionRow{
			Revision: 1,
			Configuration: connection.Configuration{
				Options: connection.Options{
					OperationDefaults: map[string]connectors.DefaultSet{
						"generation": {Values: map[string]json.RawMessage{"temperature": json.RawMessage("{invalid")}},
					},
				},
			},
		}
		if _, err := diffRevisions(bad, &revisionRow{Revision: 2}); err == nil {
			t.Fatal("diffRevisions accepted malformed OperationDefaults JSON without error")
		}
	})
}
