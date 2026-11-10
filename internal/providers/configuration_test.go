package providers

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

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
