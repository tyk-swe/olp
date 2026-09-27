package runtime

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestMountedProfileCannotChangePublishedSemanticsOrCredentialIdentity(t *testing.T) {
	id, slotID, credentialID, networkID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	provider := Provider{ID: id, Enabled: true, Kind: "openai", AuthMode: "api_key", ProfileID: "openai-chat", ProfileRevision: "1", Endpoint: "https://api.openai.com/v1", DefaultSlotID: slotID, Slots: []Slot{{ID: slotID, Enabled: true, CredentialID: &credentialID}}, SemanticHeaders: map[string]string{"OpenAI-Beta": "fixture"}, Network: &egress.ConnectionOptions{CredentialID: networkID}}
	cfg := Configuration{Kind: provider.Kind, AuthMode: provider.AuthMode, ProfileID: provider.ProfileID, ProfileRevision: provider.ProfileRevision, Endpoint: provider.Endpoint}
	cfg.Options.SemanticHeaders = map[string]string{"OpenAI-Beta": "fixture"}
	cfg.Options.Network = &egress.ConnectionOptions{CredentialID: networkID}
	cfg.Options.CredentialHeaders = []string{}
	cfg.Options.ParameterDefaults = map[string]json.RawMessage{}
	mounted := MountedProvider{Configuration: cfg, Credential: []byte("api-secret"), NetworkCredential: []byte("network-secret")}
	snapshot := Snapshot{Providers: map[string]Provider{id: provider}}
	secrets, err := installMounted(&snapshot, map[string]MountedProvider{id: mounted})
	if err != nil {
		t.Fatal(err)
	}
	if string(secrets[credentialID]) != "api-secret" || string(secrets[networkID]) != "network-secret" {
		t.Fatal("network identity not bound to published authority")
	}
	encoded, _ := json.Marshal(mounted)
	if string(encoded) == "" || bytes.Contains(encoded, []byte("api-secret")) || bytes.Contains(encoded, []byte("network-secret")) {
		t.Fatal("mounted secrets were serialized")
	}
	mounted.Configuration.Options.SemanticHeaders = map[string]string{"OpenAI-Beta": "changed"}
	if _, err := installMounted(&snapshot, map[string]MountedProvider{id: mounted}); err == nil {
		t.Fatal("mounted semantic override was accepted")
	}
	mounted.Configuration = cfg
	mounted.Configuration.Options.Network = &egress.ConnectionOptions{CredentialID: uuid.NewString()}
	if _, err := installMounted(&snapshot, map[string]MountedProvider{id: mounted}); err == nil {
		t.Fatal("mounted network identity was not published")
	}
}

// A mounted gateway mounts only the static credential of a plugin provider:
// the plugin profile comes from the published revision.
func TestMountedPluginProviderMountsItsStaticCredential(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	manifest := abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{"https://api.acme.example"}, Profiles: []abi.Profile{{
		ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat",
		Hosting: abi.Hosting{Address: "https://api.acme.example/v1", Headers: map[string]string{"Authorization": "Token {credential}"}},
	}}}
	plugin, err := connectors.NewPluginProfile(digest, manifest, "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	id, slotID, credentialID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	provider := Provider{ID: id, Enabled: true, Kind: connectors.KindPlugin, AuthMode: connectors.AuthStaticCredential, Plugin: plugin, ProfileID: "acme-chat", ProfileRevision: digest, Endpoint: plugin.Address(), DefaultSlotID: slotID, Slots: []Slot{{ID: slotID, Enabled: true, CredentialID: &credentialID}}}
	cfg := Configuration{Kind: provider.Kind, AuthMode: provider.AuthMode, ProfileID: provider.ProfileID, ProfileRevision: digest, Endpoint: provider.Endpoint}
	snapshot := Snapshot{Providers: map[string]Provider{id: provider}}
	secrets, err := installMounted(&snapshot, map[string]MountedProvider{id: {Configuration: cfg, Credential: []byte("static-secret")}})
	if err != nil {
		t.Fatal(err)
	}
	if string(secrets[credentialID]) != "static-secret" || snapshot.Providers[id].Plugin != plugin {
		t.Fatal("the static credential was not mounted for the published plugin profile")
	}
	cfg.ProfileRevision = strings.Repeat("cd", 32)
	if _, err := installMounted(&snapshot, map[string]MountedProvider{id: {Configuration: cfg, Credential: []byte("static-secret")}}); err == nil {
		t.Fatal("a mounted file moved the provider to another plugin")
	}
}

// A published provider whose credential slots hold grants can't be served by
// a mounted gateway, which has no master key to read their access tokens.
func TestMountedGatewayRefusesProvidersWithGrants(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	manifest := abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{"https://api.acme.example"}, Profiles: []abi.Profile{{
		ID: "acme-account", Label: "Acme Account", Dialect: "openai-chat", Grant: &abi.GrantAuthentication{},
		Hosting: abi.Hosting{Address: "https://api.acme.example/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}},
	}}}
	plugin, err := connectors.NewPluginProfile(digest, manifest, "acme-account")
	if err != nil {
		t.Fatal(err)
	}
	id, slotID, credentialID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	provider := Provider{ID: id, Enabled: true, Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, Plugin: plugin, ProfileID: "acme-account", ProfileRevision: digest, Endpoint: plugin.Address(), DefaultSlotID: slotID, Slots: []Slot{{ID: slotID, Enabled: true, CredentialID: &credentialID}}}
	cfg := Configuration{Kind: provider.Kind, AuthMode: provider.AuthMode, ProfileID: provider.ProfileID, ProfileRevision: digest, Endpoint: provider.Endpoint}
	snapshot := Snapshot{Providers: map[string]Provider{id: provider}}
	if _, err := installMounted(&snapshot, map[string]MountedProvider{id: {Configuration: cfg, Credential: []byte(`{"access_token":"at"}`)}}); err == nil || !strings.Contains(err.Error(), "grants") {
		t.Fatalf("a mounted gateway installed a provider with grants: %v", err)
	}
}
