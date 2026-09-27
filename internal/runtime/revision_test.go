package runtime

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func TestDecodeProviderRevisionProfiles(t *testing.T) {
	for _, tc := range []struct {
		name, configuration string
		want                Provider
	}{
		{
			name: "automatic",
			configuration: `{"kind":"openai_compatible","auth_mode":"none","endpoint":"https://model.example/v1",
				"options":{"models":{"chat":{"context_length":8192}},"parameter_defaults":{"seed":9007199254740993}}}`,
			want: Provider{Kind: "openai_compatible", AuthMode: "none", Endpoint: "https://model.example/v1",
				Models:            map[string]json.RawMessage{"chat": json.RawMessage(`{"context_length":8192}`)},
				ParameterDefaults: map[string]json.RawMessage{"seed": json.RawMessage(`9007199254740993`)}},
		},
		{
			name: "profile with network and native defaults",
			configuration: `{"kind":"openai","auth_mode":"api_key","endpoint":"https://api.openai.com/v1",
				"profile_id":"openai-chat","profile_revision":"1","options":{
				"network":{"proxy_url":"https://proxy.example","credential_id":"network-credential"},
				"semantic_headers":{"OpenAI-Beta":"fixture"},"query_settings":{"version":"pinned"},
				"operation_defaults":{"generation":{"dialect":"openai-chat","values":{"seed":9007199254740993},"native_options":{"flag":false}}},
				"bindings":{"chat":{"model":"gpt-4.1","principal_id":"principal","snapshot":"snapshot","resource_scope":"scope","defaults":{"generation":{"dialect":"openai-chat","values":{"temperature":0}}}}},
				"credential_headers":["X-Api-Key"],"vendor_id":"openai","limits":{"requests_per_minute":60,"tokens_per_minute":120000,"max_concurrency":4}}}`,
			want: Provider{Kind: "openai", AuthMode: "api_key", Endpoint: "https://api.openai.com/v1", ProfileID: "openai-chat", ProfileRevision: "1",
				Network:         &egress.ConnectionOptions{ProxyURL: "https://proxy.example", CredentialID: "network-credential"},
				SemanticHeaders: map[string]string{"OpenAI-Beta": "fixture"}, QuerySettings: map[string]string{"version": "pinned"},
				OperationDefaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"seed": json.RawMessage(`9007199254740993`)}, NativeOptions: map[string]json.RawMessage{"flag": json.RawMessage(`false`)}}},
				Bindings:          map[string]connectors.Binding{"chat": {Model: "gpt-4.1", PrincipalID: "principal", Snapshot: "snapshot", ResourceScope: "scope", Defaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"temperature": json.RawMessage(`0`)}}}}},
				CredentialHeaders: []string{"X-Api-Key"}, VendorID: "openai",
				Limits: &Limits{RequestsPerMinute: quotaLimit(60), TokensPerMinute: quotaLimit(120000), MaxConcurrency: quotaLimit(4)}},
		},
		{
			name: "cloud hosting",
			configuration: `{"kind":"azure_openai","auth_mode":"api_key","endpoint":"https://fixture.openai.azure.com",
				"profile_id":"azure-legacy-chat","profile_revision":"1","cloud_region":"eastus","cloud_project":"project",
				"deployment":"chat-deployment","api_version":"2024-10-21","options":{"bindings":{"chat":{"deployment":"chat-deployment","region":"eastus"}}}}`,
			want: Provider{Kind: "azure_openai", AuthMode: "api_key", Endpoint: "https://fixture.openai.azure.com", ProfileID: "azure-legacy-chat", ProfileRevision: "1",
				CloudRegion: "eastus", CloudProject: "project", Deployment: "chat-deployment", APIVersion: "2024-10-21",
				Bindings: map[string]connectors.Binding{"chat": {Deployment: "chat-deployment", Region: "eastus"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := "owning-project"
			provider, err := DecodeProviderRevision(ProviderRevision{
				ID: "provider", RevisionID: "revision", Name: "historical name", State: "active", ProjectID: &project,
				Configuration: []byte(tc.configuration), Models: []byte(`[]`), Slots: []byte(`[]`),
			})
			if err != nil {
				t.Fatal(err)
			}
			tc.want.ID, tc.want.RevisionID, tc.want.Name = "provider", "revision", "historical name"
			tc.want.Enabled, tc.want.ProjectID, tc.want.Capabilities = true, &project, []Capability{}
			if !reflect.DeepEqual(provider, tc.want) {
				t.Fatalf("provider = %+v, want %+v", provider, tc.want)
			}
		})
	}
}

func TestDecodeProviderRevisionCertifiedCapabilitiesAndCredentialSlots(t *testing.T) {
	revision := ProviderRevision{
		State: "disabled", Configuration: []byte(`{"kind":"openai"}`),
		Models: []byte(`[
			{"upstream_model":"model-b","capabilities":[
				{"operation":"generation","surface":"openai","mode":"unary","source":"certified"},
				{"operation":"generation","surface":"openai","mode":"streaming","source":"discovered"}]},
			{"upstream_model":"model-a","capabilities":[
				{"operation":"embeddings","surface":"openai","mode":"unary","source":"certified","certified_at":"2026-09-01T12:00:00Z"},
				{"operation":"generation","surface":"openai","mode":"unary","source":"manual"}]}]`),
		Slots: []byte(`[
			{"id":"restricted","name":"backup","enabled":false,"priority":2,"weight":7,"credential_id":"old-key","credential_version":3,
				"allowed_models":["model-b"],"allowed_routes":["chat"],"allowed_api_keys":["client-key"],"requests_per_minute":10,"tokens_per_minute":1000,"max_concurrency":2},
			{"id":"primary","name":"default","enabled":true,"priority":1,"weight":100,"credential_id":"active-key","credential_version":4,"default":true}]`),
	}
	provider, err := DecodeProviderRevision(revision)
	if err != nil {
		t.Fatal(err)
	}
	wantCapabilities := []Capability{
		{Model: "model-b", Operation: "generation", Surface: "openai", Mode: "unary"},
		{Model: "model-a", Operation: "embeddings", Surface: "openai", Mode: "unary"},
	}
	if provider.Enabled || !reflect.DeepEqual(provider.Capabilities, wantCapabilities) {
		t.Fatalf("state or certification changed: %+v", provider)
	}
	oldID, activeID, oldVersion, activeVersion := "old-key", "active-key", 3, 4
	wantSlots := []Slot{
		{ID: "restricted", Name: "backup", Enabled: false, Priority: 2, Weight: 7, CredentialID: &oldID, CredentialVersion: &oldVersion,
			AllowedModels: []string{"model-b"}, AllowedRoutes: []string{"chat"}, AllowedAPIKeys: []string{"client-key"}, RequestsPerMinute: quotaLimit(10), TokensPerMinute: quotaLimit(1000), MaxConcurrency: quotaLimit(2)},
		{ID: "primary", Name: "default", Enabled: true, Priority: 1, Weight: 100, CredentialID: &activeID, CredentialVersion: &activeVersion},
	}
	if !reflect.DeepEqual(provider.Slots, wantSlots) || provider.DefaultSlotID != "primary" || provider.ActiveCredential == nil || *provider.ActiveCredential != activeID {
		t.Fatalf("slots or default changed: %+v", provider)
	}
	// A default slot can deliberately carry no secret (for example workload identity).
	revision.Slots = []byte(`[{"id":"workload","enabled":true,"default":true,"credential_id":null}]`)
	provider, err = DecodeProviderRevision(revision)
	if err != nil || provider.DefaultSlotID != "workload" || provider.ActiveCredential != nil {
		t.Fatalf("credential-free default = %+v, %v", provider, err)
	}
}

// A revision records the principal its slots observe, which the serving
// provider carries into the connector configuration serving identity reads.
func TestDecodeProviderRevisionCarriesTheObservedPrincipal(t *testing.T) {
	provider, err := DecodeProviderRevision(ProviderRevision{
		ID: "provider", RevisionID: "revision", State: "active", Configuration: []byte(`{"kind":"openai"}`), Models: []byte(`[]`),
		Slots: []byte(`[
			{"id":"retired","enabled":false,"weight":1,"credential_id":"revoked-grant","credential_version":1},
			{"id":"primary","enabled":true,"weight":1,"credential_id":"current-grant","credential_version":2,"default":true,"observed_principal":"operator@example.com"}]`),
	})
	if err != nil || provider.ObservedPrincipal != "operator@example.com" || provider.Connector().ServingPrincipal("any-model") != "operator@example.com" {
		t.Fatalf("provider %+v: %v", provider, err)
	}
}

func TestDecodeRevisionSerializationAndPublicationLimits(t *testing.T) {
	for _, raw := range []string{`null`, `[]`} {
		provider, err := DecodeProviderRevision(ProviderRevision{ID: "provider", RevisionID: "revision", State: "active", Configuration: []byte(`{"kind":"openai","options":{"limits":{}}}`), Models: []byte(raw), Slots: []byte(raw)})
		if err != nil {
			t.Fatal(err)
		}
		if provider.Limits == nil || provider.Capabilities == nil || provider.Slots != nil {
			t.Fatalf("stored presence changed: %+v", provider)
		}
		want := `{"id":"provider","name":"","kind":"openai","enabled":true,"active_credential":null,"capabilities":[],"revision_id":"revision","limits":{}}`
		assertRevisionJSON(t, provider, want)
		provider.Limits = publishedLimits(provider.Limits)
		assertRevisionJSON(t, provider, strings.Replace(want, `,"limits":{}`, "", 1))
	}
	for _, tc := range []struct{ operations, want string }{{`null`, `null`}, {`[]`, `[]`}} {
		route, err := DecodeRouteRevision(RouteRevision{ID: "route", Slug: "chat", Operations: []byte(tc.operations), Targets: []byte(`[]`)})
		if err != nil {
			t.Fatal(err)
		}
		assertRevisionJSON(t, route, `{"id":"route","slug":"chat","operations":`+tc.want+`,"overall_timeout":0,"max_attempts":0,"targets":null,"routing_id":"route","published_at":"0001-01-01T00:00:00Z","fidelity":{"mode":"strict"}}`)
	}
}

func assertRevisionJSON(t *testing.T, value any, want string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != want {
		t.Fatalf("serialized revision = %s, want %s", encoded, want)
	}
}

func TestDecodeRouteRevisionPreservesTargetsPoliciesAndMetadata(t *testing.T) {
	publishedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.FixedZone("offset", -4*60*60))
	project := "project"
	route, err := DecodeRouteRevision(RouteRevision{
		ID: "route", Slug: "chat", RevisionID: "revision", Revision: 3, OverallTimeout: 60000, MaxAttempts: 2,
		PublishedAt: publishedAt, ProjectID: &project, Operations: []byte(`["generation","embeddings"]`),
		Targets: []byte(`[
			{"id":"target-b","provider_id":"provider-b","provider_model":"model-b","provider_model_id":"stable-b","priority":2,"weight":7,"timeout_ms":5000,"position":9},
			{"id":"target-a","provider_id":"provider-a","provider_model":"model-a","provider_model_id":"stable-a","priority":1,"weight":100,"timeout_ms":10000,"position":0}]`),
		Policy:        []byte(`{"allowed_strategies":["weighted"],"constraints":{"only":[]},"defaults":{"order":["provider:provider-b"]}}`),
		ContentPolicy: []byte(`{"rules":[{"id":"secret","phase":"output","action":"block","pattern":"secret"}]}`),
		Fidelity:      []byte(`{"mode":"transformed"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTargets := []Target{
		{ID: "target-b", ProviderID: "provider-b", ProviderModel: "model-b", RoutingID: "stable-b", Priority: 2, Weight: 7, Timeout: 5000},
		{ID: "target-a", ProviderID: "provider-a", ProviderModel: "model-a", RoutingID: "stable-a", Priority: 1, Weight: 100, Timeout: 10000},
	}
	if route.ID != "route" || route.Slug != "chat" || route.RevisionID != "revision" || route.Revision != 3 || route.RoutingID != "route" || route.ProjectID == nil || *route.ProjectID != project || route.OverallTimeout != 60000 || route.MaxAttempts != 2 {
		t.Fatalf("metadata changed: %+v", route)
	}
	if !reflect.DeepEqual(route.Targets, wantTargets) || !reflect.DeepEqual(route.Operations, []string{"generation", "embeddings"}) || !route.PublishedAt.Equal(publishedAt) || route.PublishedAt.Location() != time.UTC {
		t.Fatalf("routing or timestamp changed: %+v", route)
	}
	if route.Fidelity.Strict() || route.Policy == nil || route.Policy.Constraints.Only == nil || len(route.Policy.Constraints.Only) != 0 || route.Policy.Defaults.Order[0] != "provider:provider-b" || route.ContentPolicy == nil || route.ContentPolicy.Rules[0].Pattern != "secret" {
		t.Fatalf("policy changed: %+v", route)
	}
	for _, fidelity := range []string{"", `null`, `{}`, `{"mode":"strict"}`} {
		route, err := DecodeRouteRevision(RouteRevision{Operations: []byte(`[]`), Targets: []byte(`null`), Policy: []byte(`null`), ContentPolicy: []byte(`null`), Fidelity: []byte(fidelity)})
		if err != nil || route.Fidelity.Mode != FidelityStrict || route.Policy != nil || route.ContentPolicy != nil || route.Targets != nil {
			t.Fatalf("default fidelity %q = %+v, %v", fidelity, route, err)
		}
	}
}

func TestDecodeRevisionMalformedDocumentsKeepErrorCauses(t *testing.T) {
	for _, field := range []string{"configuration", "models", "slots", "operations", "targets", "policy", "content policy"} {
		for _, raw := range []string{`{`, `42`} {
			t.Run(field+raw, func(t *testing.T) {
				provider := ProviderRevision{ID: "provider", Configuration: []byte(`{}`), Models: []byte(`[]`), Slots: []byte(`[]`)}
				route := RouteRevision{Slug: "chat", Operations: []byte(`[]`), Targets: []byte(`[]`)}
				var err error
				switch field {
				case "configuration", "models", "slots":
					fields := map[string]*[]byte{"configuration": &provider.Configuration, "models": &provider.Models, "slots": &provider.Slots}
					*fields[field] = []byte(raw)
					_, err = DecodeProviderRevision(provider)
				default:
					fields := map[string]*[]byte{"operations": &route.Operations, "targets": &route.Targets, "policy": &route.Policy, "content policy": &route.ContentPolicy}
					*fields[field] = []byte(raw)
					_, err = DecodeRouteRevision(route)
				}
				var syntax *json.SyntaxError
				var mismatch *json.UnmarshalTypeError
				if raw == `{` && !errors.As(err, &syntax) || raw == `42` && !errors.As(err, &mismatch) {
					t.Fatalf("lost JSON error cause: %v", err)
				}
			})
		}
	}
	_, err := DecodeRouteRevision(RouteRevision{Slug: "chat", Operations: []byte(`[]`), Targets: []byte(`[]`), Fidelity: []byte(`{"mode":"unknown"}`)})
	if !errors.Is(err, errFidelityMode) || !strings.Contains(err.Error(), "route chat fidelity:") {
		t.Fatalf("lost fidelity error cause: %v", err)
	}
}
