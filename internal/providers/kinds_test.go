package providers

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/vendors"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestKindAuthModesAgreeWithTheirAuthenticators(t *testing.T) {
	for _, kind := range kinds {
		for _, mode := range kind.AuthModes {
			// A grant's credential versions come from grant enrollment rather
			// than a pasted secret, but it authorizes with one all the same.
			if connectors.SecretRequired(mode.Mode) != (mode.Credential != "forbidden") || (mode.Credential == "grant") != (mode.Mode == connectors.AuthGrant) {
				t.Errorf("%s %s: catalog credential %q disagrees with its authenticator", kind.Kind, mode.Mode, mode.Credential)
			}
		}
	}
	cfg := Configuration{Kind: KindOpenAI, AuthMode: "unregistered"}
	cfg.Normalize()
	if problem, ok := errors.AsType[*access.Problem](cfg.Validate(&egress.Policy{})); !ok || problem.Field != "configuration.auth_mode" {
		t.Fatal("an unknown auth mode passed validation")
	}
}

// A plugin provider takes its address and credential placement from the
// plugin profile it pins; no kind default applies to it.
func TestPluginProvidersTakeNoKindDefaults(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	cfg := Configuration{Kind: KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileID: "acme-chat", ProfileRevision: digest}
	cfg.Normalize()
	if cfg.Endpoint != nil || cfg.Options.VendorID != nil {
		t.Fatalf("normalization applied a kind default: %v %v", cfg.Endpoint, cfg.Options.VendorID)
	}
	if problem, ok := errors.AsType[*access.Problem](cfg.Validate(&egress.Policy{})); !ok || problem.Field != "configuration.profile_revision" {
		t.Fatal("an unresolved plugin profile passed validation")
	}
	pinAcme(t, &cfg, "https://api.acme.example", abi.Profile{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Hosting: abi.Hosting{Address: "https://api.acme.example/v1", Headers: map[string]string{"Authorization": "Token {credential}"}},
	})
	if err := cfg.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	vendor := cfg
	vendor.Options.VendorID = new("openai")
	if problem, ok := errors.AsType[*access.Problem](vendor.Validate(&egress.Policy{})); !ok || problem.Field != "configuration.options.vendor_id" {
		t.Fatal("a plugin provider acquired a vendor")
	}
	for _, capability := range capabilitiesFor(KindPlugin, defaultVendor(KindPlugin)) {
		if capability.Operation != OperationGeneration || capability.Surface == "bedrock" {
			t.Fatalf("the plugin kind offers %+v", capability)
		}
	}
}

// A plugin provider's options are part of its configuration: validated
// against the options its plugin profile declares, placed in its endpoint and
// part of what certification was gathered against.
func TestPluginProviderOptionsFollowTheirProfile(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	profile := abi.Profile{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Options: []abi.Option{{Name: "account", Label: "Account", Pattern: "^[a-z]+$"}, {Name: "region", Label: "Region", Optional: true, Enum: []string{"us", "eu"}}},
		Hosting: abi.Hosting{Address: "https://api.acme.example/accounts/{options.account}/v1", Headers: map[string]string{"Authorization": "Token {credential}"}},
	}
	configured := func(options map[string]string) Configuration {
		cfg := Configuration{Kind: KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileID: "acme-chat", ProfileRevision: digest, Options: Options{PluginOptions: options}}
		cfg.Normalize()
		pinAcme(t, &cfg, "https://api.acme.example", profile)
		return cfg
	}
	cfg := configured(map[string]string{"account": "acme", "region": ""})
	if err := cfg.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	if *cfg.Endpoint != "https://api.acme.example/accounts/acme/v1" || len(cfg.Options.PluginOptions) != 1 {
		t.Fatalf("configured %v with options %v", *cfg.Endpoint, cfg.Options.PluginOptions)
	}
	for name, tc := range map[string]struct {
		options map[string]string
		field   string
	}{
		"required unset":   {map[string]string{"region": "eu"}, "configuration.options.plugin_options.account"},
		"pattern mismatch": {map[string]string{"account": "Acme"}, "configuration.options.plugin_options.account"},
		"outside the enum": {map[string]string{"account": "acme", "region": "ap"}, "configuration.options.plugin_options.region"},
		"undeclared":       {map[string]string{"account": "acme", "project": "p"}, "configuration.options.plugin_options.project"},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := configured(tc.options)
			if problem, ok := errors.AsType[*access.Problem](invalid.Validate(&egress.Policy{})); !ok || problem.Field != tc.field {
				t.Fatalf("want a refusal of %s, got %v", tc.field, problem)
			}
		})
	}
	other := configured(map[string]string{"account": "other"})
	if cfg.transportFingerprint() == other.transportFingerprint() {
		t.Fatal("certification survives a change of plugin options")
	}
	builtin := Configuration{Kind: KindOpenAI, AuthMode: "api_key", Options: Options{PluginOptions: map[string]string{"account": "acme"}}}
	builtin.Normalize()
	if problem, ok := errors.AsType[*access.Problem](builtin.Validate(&egress.Policy{})); !ok || problem.Field != "configuration.options.plugin_options" {
		t.Fatalf("a built-in provider took plugin options: %v", problem)
	}
}

func TestEveryVendorContractNamesAKindThatServesIt(t *testing.T) {
	for _, contract := range vendors.All() {
		kind := kindByName(contract.Connector)
		if kind == nil {
			t.Fatalf("%s names unknown connector kind %s", contract.ID, contract.Connector)
		}
		if contract.Preset != nil && !slices.ContainsFunc(kind.AuthModes, func(a authCapability) bool { return a.Mode == contract.Preset.AuthMode }) {
			t.Fatalf("%s presets authentication %s, which %s does not accept", contract.ID, contract.Preset.AuthMode, kind.Kind)
		}
		for _, operation := range contract.Operations {
			served := false
			for _, option := range CapabilityOptions {
				served = served || option.Operation == operation && connectors.Supports(contract.Connector, contract.ID, operation, option.Surface, option.Mode)
			}
			if !served {
				t.Fatalf("%s lists %s, which its connector kind never serves", contract.ID, operation)
			}
		}
	}
}

// TestCloudKindsConfigureFromTheirDefaults covers the SageMaker and watsonx
// kinds: a region, and watsonx's project, give each its regional endpoint.
func TestCloudKindsConfigureFromTheirDefaults(t *testing.T) {
	for _, test := range []struct {
		cfg      Configuration
		endpoint string
	}{
		{Configuration{Kind: KindSageMaker, AuthMode: "default_chain", CloudRegion: new("eu-central-1")}, "https://runtime.sagemaker.eu-central-1.amazonaws.com"},
		{Configuration{Kind: KindWatsonx, AuthMode: "ibm_iam", CloudRegion: new("eu-de"), CloudProject: new("8f3b2c1d-1234-4abc-9def-0123456789ab"), APIVersion: new("2026-09-25")}, "https://eu-de.ml.cloud.ibm.com"},
	} {
		test.cfg.Normalize()
		if err := test.cfg.Validate(&egress.Policy{}); err != nil || value(test.cfg.Endpoint) != test.endpoint || value(test.cfg.Options.VendorID) != defaultVendor(test.cfg.Kind) {
			t.Fatalf("%s configuration = %s %v, %v", test.cfg.Kind, value(test.cfg.Endpoint), value(test.cfg.Options.VendorID), err)
		}
	}
}
