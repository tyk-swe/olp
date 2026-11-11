package providers

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/vendors"
)

type identity = providerconfig.Identity[*string]

func openAI(options Options) Configuration {
	return Configuration{Identity: identity{Kind: "openai", AuthMode: AuthAPIKey}, Options: options}
}

func validated(c Configuration) (Configuration, error) {
	normalized := c
	normalized.Normalize()
	err := normalized.ValidateAndNormalize(&egress.Policy{})
	return normalized, err
}

func refusedField(err error) (string, bool) {
	if problem, ok := errors.AsType[*access.Problem](err); ok {
		return problem.Field, problem.Status == 422
	}
	if invalid, ok := errors.AsType[*routing.ValidationError](err); ok {
		return invalid.Field, true
	}
	return "", false
}

func TestValidateAndNormalizeCompletesAUsableConnection(t *testing.T) {
	saved, err := validated(Configuration{
		Identity: identity{Kind: "openai", AuthMode: AuthHeaders},
		Options: Options{
			CredentialHeaders: []string{" x-tenant-key "},
			Limits:            &Limits{RequestsPerMinute: new(int64(60)), Supply: routing.Supply{PriorityShares: &routing.Shares{Critical: 40, High: 30, Normal: 20, Low: 10}, SaturationPercent: new(int64(80))}},
			Commitment:        &routing.Commitment{Amount: "250.00", Period: "month"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
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

func TestValidateAndNormalizeRefusesConnectionsThatCannotBeServedSafely(t *testing.T) {
	shares := &routing.Shares{Critical: 25, High: 25, Normal: 25, Low: 25}
	cases := map[string]struct {
		configuration Configuration
		field         string
	}{
		"an unknown connector kind":                         {Configuration{Identity: identity{Kind: "telepathy", AuthMode: AuthAPIKey, Endpoint: new("https://upstream.example")}}, ""},
		"caller credentials only the gateway could mint":    {Configuration{Identity: identity{Kind: "bedrock", AuthMode: "default_chain", CredentialSource: "caller", CloudRegion: new("us-east-1")}}, "configuration.credential_source"},
		"an authentication mode the connector lacks":        {Configuration{Identity: identity{Kind: "openai", AuthMode: "default_chain"}}, "configuration.auth_mode"},
		"no endpoint where the connector has no default":    {Configuration{Identity: identity{Kind: "azure_openai", AuthMode: AuthAPIKey, Deployment: new("gpt"), APIVersion: new("2024-10-21")}}, "configuration.endpoint"},
		"a loopback endpoint":                               {Configuration{Identity: identity{Kind: "openai", AuthMode: AuthAPIKey, Endpoint: new("https://127.0.0.1/v1")}}, "configuration.endpoint"},
		"a cloud metadata endpoint":                         {Configuration{Identity: identity{Kind: "openai", AuthMode: AuthAPIKey, Endpoint: new("https://169.254.169.254/v1")}}, "configuration.endpoint"},
		"a plain HTTP endpoint":                             {Configuration{Identity: identity{Kind: "openai", AuthMode: AuthAPIKey, Endpoint: new("http://upstream.example/v1")}}, "configuration.endpoint"},
		"cloud context on a connector without one":          {Configuration{Identity: identity{Kind: "openai", AuthMode: AuthAPIKey, CloudRegion: new("us-east-1")}}, "configuration"},
		"credential headers outside the headers mode":       {openAI(Options{CredentialHeaders: []string{"X-Tenant-Key"}}), "configuration.options.credential_headers"},
		"the headers mode without a header":                 {Configuration{Identity: identity{Kind: "openai", AuthMode: AuthHeaders}}, "configuration.options.credential_headers"},
		"a credential header that reframes the request":     {Configuration{Identity: identity{Kind: "openai", AuthMode: AuthHeaders}, Options: Options{CredentialHeaders: []string{"Transfer-Encoding"}}}, "configuration.options.credential_headers"},
		"a credential header reserved for the gateway":      {Configuration{Identity: identity{Kind: "openai", AuthMode: AuthHeaders}, Options: Options{CredentialHeaders: []string{"X-Olp-Tenant"}}}, "configuration.options.credential_headers"},
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
			_, err := validated(c.configuration)
			if field, refused := refusedField(err); !refused || field != c.field {
				t.Fatalf("got %#v, want a refusal of %q", err, c.field)
			}
		})
	}
}

func storedInputs() map[string]Configuration {
	effective := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ends := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	return map[string]Configuration{
		"null-rich-openai": openAI(Options{}),
		"limits-supply": {Identity: identity{Kind: KindAnthropic, AuthMode: AuthAPIKey}, Options: Options{Limits: &Limits{
			MaxConcurrency: new(int64(4)), RequestsPerMinute: new(int64(60)), TokensPerMinute: new(int64(100000)),
			Supply: routing.Supply{DailyCostLimit: new("10.00"), MonthlyCostLimit: new("200"), PriorityShares: &routing.Shares{Critical: 40, High: 30, Normal: 20, Low: 10}, SaturationPercent: new(int64(80))},
		}}},
		"commitment": {Identity: identity{Kind: KindBedrock, AuthMode: "default_chain", CloudRegion: new("us-east-1")},
			Options: Options{Commitment: &routing.Commitment{ID: "commitment-1", EffectiveAt: &effective, EndsAt: &ends, Amount: " 1200.50 ", Period: "month"}}},
		"health-probe": {Identity: identity{Kind: KindGemini, AuthMode: AuthAPIKey},
			Options: Options{HealthProbe: &routing.HealthProbe{IntervalSeconds: 300}, Models: map[string]json.RawMessage{"gemini-2.5-flash": json.RawMessage(`{"context_length":1048576}`)}}},
		"azure-deployment": {Identity: identity{Kind: KindAzure, AuthMode: AuthAPIKey, ProfileID: "azure-legacy-chat", ProfileRevision: connectors.ProfileRevision,
			Endpoint: new("https://example.openai.azure.com"), Deployment: new("chat-deployment"), APIVersion: new("2024-10-21")},
			Options: Options{
				Network:           &egress.ConnectionOptions{CredentialID: "6f1c2d3e-4b5a-4c6d-8e7f-9a0b1c2d3e4f", ConnectTimeoutMS: new(int64(2000)), MaxConnsPerHost: new(8)},
				SemanticHeaders:   map[string]string{"OpenAI-Beta": "assistants=v2"},
				QuerySettings:     map[string]string{"api-version": "2024-10-21"},
				OperationDefaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"temperature": json.RawMessage(`0.2`)}}},
				Bindings:          map[string]connectors.Binding{"gpt-4o": {Deployment: "gpt-4o-deployment", PrincipalID: "principal-1", Snapshot: "2024-08-06", ResourceScope: "scope-1"}},
				Models:            map[string]json.RawMessage{"gpt-4o": json.RawMessage(`{"context_length":128000}`)},
			}},
		"vertex-batch": {Identity: identity{Kind: KindVertex, AuthMode: "service_account", ProfileID: "vertex-batch-prediction", ProfileRevision: connectors.ProfileRevision,
			CloudRegion: new("us-central1"), CloudProject: new("acme-project"), BatchStorage: &connectors.BatchStorage{InputURIPrefix: "gs://acme-batch/in/", OutputURIPrefix: "gs://acme-batch/out/"}},
			Options: Options{
				TuningDatasetURIPrefixes: []string{"gs://acme-tuning/datasets/"},
				DocumentURIPrefixes:      []string{"gs://acme-docs/"},
				QuerySettings:            map[string]string{"$xgafv": "2"},
				Bindings:                 map[string]connectors.Binding{"gemini-2.5-pro": {Region: "us-central1"}},
			}},
		"credential-headers": {Identity: identity{CredentialSource: "caller", Kind: KindOpenAICompatible, AuthMode: AuthHeaders, Endpoint: new("https://llm.example.com/v1")},
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

func storedBytes(t *testing.T, c Configuration) string {
	t.Helper()
	encoded, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func readStored(t *testing.T, name string) Configuration {
	t.Helper()
	var stored Configuration
	if err := json.Unmarshal(golden(t, name), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Normalize()
	if err := stored.pinned(testdata(t, "installed-plugin.json")); err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestPrepareConfigurationStoresTheGoldenBytes(t *testing.T) {
	t.Parallel()
	for name, input := range storedInputs() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			prepared, err := PrepareConfiguration(t.Context(), nil, nil, &egress.Policy{}, input)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := storedBytes(t, prepared.Configuration()), string(golden(t, name)); got != want {
				t.Fatalf("stored\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestStoredConfigurationRevalidatesToTheSameBytes(t *testing.T) {
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
			if err := stored.ValidateAndNormalize(&egress.Policy{}); err != nil {
				t.Fatal(err)
			}
			if got := storedBytes(t, stored); got != want {
				t.Fatalf("revalidated\n%s\nwant\n%s", got, want)
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
			validated, served := reflect.ValueOf(stored.transport()), reflect.ValueOf(servedConnector(t, name))
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
