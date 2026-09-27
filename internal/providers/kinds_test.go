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
	manifest, _ := json.Marshal(abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{"https://api.acme.example"}, Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Hosting: abi.Hosting{Address: "https://api.acme.example/v1", Headers: map[string]string{"Authorization": "Token {credential}"}},
	}}})
	if err := cfg.pinned(manifest); err != nil {
		t.Fatal(err)
	}
	cfg.Endpoint = new(cfg.plugin.Address())
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
