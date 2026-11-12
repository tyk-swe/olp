package connection

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/vendors"
)

type identity = providerconfig.Identity[*string]

func openAI(options Options) Configuration {
	return Configuration{Identity: identity{Kind: "openai", AuthMode: connectors.AuthAPIKey}, Options: options}
}

type installedPlugin []byte

func (installed installedPlugin) Profile(_ context.Context, digest, profileID string) (*connectors.PluginProfile, error) {
	return connectors.DecodePluginProfile(digest, installed, profileID)
}

func prepare(t *testing.T, c Configuration) (Prepared, error) {
	t.Helper()
	return Prepare(t.Context(), Policy{Egress: &egress.Policy{}, Plugins: installedPlugin(testdata(t, "installed-plugin.json"))}, c)
}

func refusal(err error) (string, bool) {
	if invalid, ok := errors.AsType[*routing.ValidationError](err); ok {
		return invalid.Field, true
	}
	if refused, ok := errors.AsType[*routing.Refusal](err); ok {
		return refused.Code, true
	}
	return "", false
}

func TestPrepareCompletesAUsableConnection(t *testing.T) {
	prepared, err := prepare(t, Configuration{
		Identity: identity{Kind: "openai", AuthMode: connectors.AuthHeaders},
		Options: Options{
			CredentialHeaders: []string{" x-tenant-key "},
			Limits:            &Limits{RequestsPerMinute: new(int64(60)), Supply: routing.Supply{PriorityShares: &routing.Shares{Critical: 40, High: 30, Normal: 20, Low: 10}, SaturationPercent: new(int64(80))}},
			Commitment:        &routing.Commitment{Amount: "250.00", Period: "month"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	saved := prepared.Document()
	if want := connectors.DefaultEndpoint("openai", "", ""); saved.Endpoint == nil || *saved.Endpoint != want || want == "" {
		t.Fatalf("endpoint %v, want the connector's default %q", saved.Endpoint, want)
	}
	if want := vendors.DefaultFor("openai"); saved.Options.VendorID == nil || *saved.Options.VendorID != want {
		t.Fatalf("vendor %v, want the connector's default %q", saved.Options.VendorID, want)
	}
	if !slices.Equal(saved.Options.CredentialHeaders, []string{"X-Tenant-Key"}) {
		t.Fatalf("credential headers %q, want the canonical name", saved.Options.CredentialHeaders)
	}
}

func TestPrepareRefusesConnectionsThatCannotBeServedSafely(t *testing.T) {
	shares := &routing.Shares{Critical: 25, High: 25, Normal: 25, Low: 25}
	cases := map[string]struct {
		configuration Configuration
		refused       string
	}{
		"an unknown connector kind":                         {Configuration{Identity: identity{Kind: "telepathy", AuthMode: connectors.AuthAPIKey, Endpoint: new("https://upstream.example")}}, "provider_kind_unavailable"},
		"caller credentials only the gateway could mint":    {Configuration{Identity: identity{Kind: "bedrock", AuthMode: "default_chain", CredentialSource: "caller", CloudRegion: new("us-east-1")}}, "configuration.credential_source"},
		"an authentication mode the connector lacks":        {Configuration{Identity: identity{Kind: "openai", AuthMode: "default_chain"}}, "configuration.auth_mode"},
		"no endpoint where the connector has no default":    {Configuration{Identity: identity{Kind: "azure_openai", AuthMode: connectors.AuthAPIKey, Deployment: new("gpt"), APIVersion: new("2024-10-21")}}, "configuration.endpoint"},
		"a loopback endpoint":                               {Configuration{Identity: identity{Kind: "openai", AuthMode: connectors.AuthAPIKey, Endpoint: new("https://127.0.0.1/v1")}}, "configuration.endpoint"},
		"a cloud metadata endpoint":                         {Configuration{Identity: identity{Kind: "openai", AuthMode: connectors.AuthAPIKey, Endpoint: new("https://169.254.169.254/v1")}}, "configuration.endpoint"},
		"a plain HTTP endpoint":                             {Configuration{Identity: identity{Kind: "openai", AuthMode: connectors.AuthAPIKey, Endpoint: new("http://upstream.example/v1")}}, "configuration.endpoint"},
		"cloud context on a connector without one":          {Configuration{Identity: identity{Kind: "openai", AuthMode: connectors.AuthAPIKey, CloudRegion: new("us-east-1")}}, "configuration"},
		"credential headers outside the headers mode":       {openAI(Options{CredentialHeaders: []string{"X-Tenant-Key"}}), "configuration.options.credential_headers"},
		"the headers mode without a header":                 {Configuration{Identity: identity{Kind: "openai", AuthMode: connectors.AuthHeaders}}, "configuration.options.credential_headers"},
		"a credential header that reframes the request":     {Configuration{Identity: identity{Kind: "openai", AuthMode: connectors.AuthHeaders}, Options: Options{CredentialHeaders: []string{"Transfer-Encoding"}}}, "configuration.options.credential_headers"},
		"a credential header reserved for the gateway":      {Configuration{Identity: identity{Kind: "openai", AuthMode: connectors.AuthHeaders}, Options: Options{CredentialHeaders: []string{"X-Olp-Tenant"}}}, "configuration.options.credential_headers"},
		"a vendor served by another connector":              {openAI(Options{VendorID: new(vendors.DefaultFor("anthropic"))}), "configuration.options.vendor_id"},
		"a vendor nobody knows":                             {openAI(Options{VendorID: new("telepathy")}), "configuration.options.vendor_id"},
		"model metadata that is not an object":              {openAI(Options{Models: map[string]json.RawMessage{"fixture-model": json.RawMessage(`null`)}}), "configuration.options.models"},
		"a network credential that is not a canonical UUID": {openAI(Options{Network: &egress.ConnectionOptions{CredentialID: "6BA7B810-9DAD-11D1-80B4-00C04FD430C8"}}), "configuration.options.network.credential_id"},
		"plugin options on a built-in connector":            {openAI(Options{PluginOptions: map[string]string{"region": "eu"}}), "configuration.options.plugin_options"},
		"a commitment that owes nothing":                    {openAI(Options{Commitment: &routing.Commitment{Amount: "0.00", Period: "month"}}), "configuration.options.commitment.amount"},
		"a commitment over an unknown period":               {openAI(Options{Commitment: &routing.Commitment{Amount: "250.00", Period: "year"}}), "configuration.options.commitment.period"},
		"a negative request limit":                          {openAI(Options{Limits: &Limits{RequestsPerMinute: new(int64(-1))}}), "configuration.options.limits"},
		"priority shares with no quota to divide":           {openAI(Options{Limits: &Limits{Supply: routing.Supply{PriorityShares: shares, SaturationPercent: new(int64(80))}}}), "configuration.options.limits.priority_shares"},
		"priority shares that leave part of the quota out":  {openAI(Options{Limits: &Limits{RequestsPerMinute: new(int64(60)), Supply: routing.Supply{PriorityShares: &routing.Shares{Critical: 50}, SaturationPercent: new(int64(80))}}}), "configuration.options.limits.priority_shares"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := prepare(t, c.configuration)
			if refused, ok := refusal(err); !ok || refused != c.refused {
				t.Fatalf("got %#v, want a refusal of %q", err, c.refused)
			}
		})
	}
}

func storedInputs() map[string]Configuration {
	effective := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ends := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	return map[string]Configuration{
		"null-rich-openai": openAI(Options{}),
		"limits-supply": {Identity: identity{Kind: connectors.KindAnthropic, AuthMode: connectors.AuthAPIKey}, Options: Options{Limits: &Limits{
			MaxConcurrency: new(int64(4)), RequestsPerMinute: new(int64(60)), TokensPerMinute: new(int64(100000)),
			Supply: routing.Supply{DailyCostLimit: new("10.00"), MonthlyCostLimit: new("200"), PriorityShares: &routing.Shares{Critical: 40, High: 30, Normal: 20, Low: 10}, SaturationPercent: new(int64(80))},
		}}},
		"commitment": {Identity: identity{Kind: connectors.KindBedrock, AuthMode: "default_chain", CloudRegion: new("us-east-1")},
			Options: Options{Commitment: &routing.Commitment{ID: "commitment-1", EffectiveAt: &effective, EndsAt: &ends, Amount: " 1200.50 ", Period: "month"}}},
		"health-probe": {Identity: identity{Kind: connectors.KindGemini, AuthMode: connectors.AuthAPIKey},
			Options: Options{HealthProbe: &routing.HealthProbe{IntervalSeconds: 300}, Models: map[string]json.RawMessage{"gemini-2.5-flash": json.RawMessage(`{"context_length":1048576}`)}}},
		"azure-deployment": {Identity: identity{Kind: connectors.KindAzureOpenAI, AuthMode: connectors.AuthAPIKey, ProfileID: "azure-legacy-chat", ProfileRevision: connectors.ProfileRevision,
			Endpoint: new("https://example.openai.azure.com"), Deployment: new("chat-deployment"), APIVersion: new("2024-10-21")},
			Options: Options{
				Network:           &egress.ConnectionOptions{CredentialID: "6f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f", ConnectTimeoutMS: new(int64(2000)), MaxConnsPerHost: new(8)},
				SemanticHeaders:   map[string]string{"OpenAI-Beta": "assistants=v2"},
				QuerySettings:     map[string]string{"api-version": "2024-10-21"},
				OperationDefaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"temperature": json.RawMessage(`0.2`)}}},
				Bindings:          map[string]connectors.Binding{"gpt-4o": {Deployment: "gpt-4o-deployment", PrincipalID: "principal-1", Snapshot: "2024-08-06", ResourceScope: "scope-1"}},
				Models:            map[string]json.RawMessage{"gpt-4o": json.RawMessage(`{"context_length":128000}`)},
			}},
		"vertex-batch": {Identity: identity{Kind: connectors.KindVertexAI, AuthMode: "service_account", ProfileID: "vertex-batch-prediction", ProfileRevision: connectors.ProfileRevision,
			CloudRegion: new("us-central1"), CloudProject: new("acme-project"), BatchStorage: &connectors.BatchStorage{InputURIPrefix: "gs://acme-batch/in/", OutputURIPrefix: "gs://acme-batch/out/"}},
			Options: Options{
				TuningDatasetURIPrefixes: []string{"gs://acme-tuning/datasets/"},
				DocumentURIPrefixes:      []string{"gs://acme-docs/"},
				QuerySettings:            map[string]string{"$xgafv": "2"},
				Bindings:                 map[string]connectors.Binding{"gemini-2.5-pro": {Region: "us-central1"}},
			}},
		"credential-headers": {Identity: identity{CredentialSource: "caller", Kind: connectors.KindOpenAICompatible, AuthMode: connectors.AuthHeaders, Endpoint: new("https://llm.example.com/v1")},
			Options: Options{VendorID: new("groq"), CredentialHeaders: []string{"x-tenant-key"}, ParameterDefaults: map[string]json.RawMessage{"temperature": json.RawMessage(`0.5`)}}},
	}
}

func storedNames() []string {
	return append(slices.Sorted(maps.Keys(storedInputs())), "plugin")
}

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func golden(t *testing.T, name string) []byte {
	t.Helper()
	return testdata(t, "configuration-"+name+".golden.json")
}

func storedBytes(t *testing.T, c any) string {
	t.Helper()
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func readStored(t *testing.T, name string) Prepared {
	t.Helper()
	stored, err := Load(golden(t, name), testdata(t, "installed-plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestPrepareStoresTheGoldenBytes(t *testing.T) {
	t.Parallel()
	for name, input := range storedInputs() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prepared, err := prepare(t, input)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := storedBytes(t, prepared), string(golden(t, name)); got != want {
				t.Fatalf("stored\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestStoredConfigurationLoadsAndPreparesToTheSameBytes(t *testing.T) {
	t.Parallel()
	for _, name := range storedNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := string(golden(t, name))
			var decoded Configuration
			if err := json.Unmarshal([]byte(want), &decoded); err != nil {
				t.Fatal(err)
			}
			if got := storedBytes(t, decoded); got != want {
				t.Fatalf("re-encoded\n%s\nwant\n%s", got, want)
			}
			stored := readStored(t, name)
			if got := storedBytes(t, stored); got != want {
				t.Fatalf("loaded\n%s\nwant\n%s", got, want)
			}
			prepared, err := prepare(t, stored.Document())
			if err != nil {
				t.Fatal(err)
			}
			if got := storedBytes(t, prepared); got != want {
				t.Fatalf("prepared again\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func servedConnector(t *testing.T, name string) connectors.Config {
	t.Helper()
	provider, err := routing.DecodeProviderRevision(routing.ProviderRevision{
		ID: "provider", RevisionID: "revision", Name: name, State: "active",
		Configuration: golden(t, name), Models: []byte(`[]`), Slots: []byte(`[]`), InstalledPlugin: testdata(t, "installed-plugin.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider.Connector()
}

func TestStoredConfigurationServesTheConnectorItWasValidatedAs(t *testing.T) {
	t.Parallel()
	runtimeOnly := []string{"CredentialSource", "ObservedPrincipal", "SigningService"}
	carried := slices.DeleteFunc(reflect.VisibleFields(reflect.TypeFor[connectors.Config]()), func(field reflect.StructField) bool {
		return slices.Contains(runtimeOnly, field.Name)
	})
	for _, name := range storedNames() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stored := readStored(t, name)
			validated, served := reflect.ValueOf(stored.Transport()), reflect.ValueOf(servedConnector(t, name))
			for _, field := range carried {
				if want, got := validated.FieldByIndex(field.Index).Interface(), served.FieldByIndex(field.Index).Interface(); !reflect.DeepEqual(got, want) {
					t.Errorf("%s served %#v, validated %#v", field.Name, got, want)
				}
			}
		})
	}
	for _, field := range carried {
		populated := slices.ContainsFunc(storedNames(), func(name string) bool {
			return !reflect.ValueOf(servedConnector(t, name)).FieldByIndex(field.Index).IsZero()
		})
		if !populated {
			t.Errorf("no stored configuration sets %s, so the served and validated copies are never compared on it", field.Name)
		}
	}
}
