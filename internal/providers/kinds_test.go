package providers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestKindAuthModesAgreeWithTheirAuthenticators(t *testing.T) {
	for _, kind := range kinds {
		for _, mode := range kind.AuthModes {
			if connectors.SecretRequired(mode.Mode) != (mode.Credential == "required") {
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
	manifest, _ := json.Marshal(connectors.InstalledPlugin{Manifest: abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{"https://api.acme.example"}, Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Hosting: abi.Hosting{Address: "https://api.acme.example/v1", Headers: map[string]string{"Authorization": "Token {credential}"}},
	}}}})
	if err := cfg.pinned(manifest); err != nil {
		t.Fatal(err)
	}
	cfg.Endpoint = new(cfg.plugin.Address(nil))
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
	manifest, _ := json.Marshal(connectors.InstalledPlugin{Manifest: abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{"https://api.acme.example"}, Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Options: []abi.Option{{Name: "account", Label: "Account", Pattern: "^[a-z]+$"}, {Name: "region", Label: "Region", Optional: true, Enum: []string{"us", "eu"}}},
		Hosting: abi.Hosting{Address: "https://api.acme.example/accounts/{options.account}/v1", Headers: map[string]string{"Authorization": "Token {credential}"}},
	}}}})
	configured := func(options map[string]string) Configuration {
		cfg := Configuration{Kind: KindPlugin, AuthMode: connectors.AuthStaticCredential, ProfileID: "acme-chat", ProfileRevision: digest, Options: Options{PluginOptions: options}}
		cfg.Normalize()
		if err := cfg.pinned(manifest); err != nil {
			t.Fatal(err)
		}
		cfg.Endpoint = new(cfg.plugin.Address(cfg.Options.PluginOptions))
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
