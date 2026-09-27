package providers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
)

func TestMountedNetworkSecretStaysOutOfConfiguration(t *testing.T) {
	directory := t.TempDir()
	secretFile := filepath.Join(directory, "network.json")
	if err := os.WriteFile(secretFile, []byte(`{"proxy_username":"fixture","proxy_password":"private"}`), 0600); err != nil {
		t.Fatal(err)
	}
	providerID, credentialID := uuid.NewString(), uuid.NewString()
	document := map[string]any{"providers": []any{map[string]any{"provider_id": providerID, "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "none", "endpoint": "https://upstream.example/v1", "profile_id": "compatible-chat", "profile_revision": "1", "options": map[string]any{"network": map[string]any{"proxy_url": "https://proxy.example:443", "credential_id": credentialID}}}, "network_credential_file": secretFile}}}
	data, _ := json.Marshal(document)
	file := filepath.Join(directory, "connectors.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := LoadMounted(file, &egress.Policy{})
	if err != nil {
		t.Fatal(err)
	}
	entry := entries[providerID]
	if entry.Configuration.Options.Network.CredentialID != credentialID || string(entry.NetworkCredential) != `{"proxy_username":"fixture","proxy_password":"private"}` {
		t.Fatal("mounted reference/material mismatch")
	}
}

// A plugin provider's mounted entry supplies its static credential; the
// gateway matches the rest against the published revision.
func TestMountedPluginProviderSuppliesItsCredential(t *testing.T) {
	directory := t.TempDir()
	secretFile := filepath.Join(directory, "credential")
	if err := os.WriteFile(secretFile, []byte("static-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	providerID := uuid.NewString()
	configuration := map[string]any{"kind": "plugin", "auth_mode": "static_credential", "endpoint": "https://api.acme.example/v1", "profile_id": "acme-chat", "profile_revision": strings.Repeat("ab", 32)}
	for name, entry := range map[string]map[string]any{
		"with its credential":    {"provider_id": providerID, "configuration": configuration, "credential_file": secretFile},
		"without its credential": {"provider_id": providerID, "configuration": configuration},
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(map[string]any{"providers": []any{entry}})
			file := filepath.Join(directory, "connectors.json")
			if err := os.WriteFile(file, data, 0600); err != nil {
				t.Fatal(err)
			}
			entries, err := LoadMounted(file, &egress.Policy{})
			if _, mounted := entry["credential_file"]; !mounted {
				if err == nil {
					t.Fatal("a static credential mode mounted no credential")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mounted := entries[providerID]; string(mounted.Credential) != "static-secret" || mounted.Configuration.Options.VendorID != "" {
				t.Fatalf("mounted %+v", mounted.Configuration)
			}
		})
	}
}

// A grant is refreshed beneath its credential version in the database, so a
// gateway without the master key refuses to mount a provider that
// authenticates with one, with or without a credential file.
func TestMountedConnectorsRefuseGrants(t *testing.T) {
	directory := t.TempDir()
	secretFile := filepath.Join(directory, "credential")
	if err := os.WriteFile(secretFile, []byte(`{"access_token":"at"}`), 0600); err != nil {
		t.Fatal(err)
	}
	configuration := map[string]any{"kind": "plugin", "auth_mode": "grant", "endpoint": "https://api.acme.example/v1", "profile_id": "acme-account", "profile_revision": strings.Repeat("ab", 32)}
	for _, entry := range []map[string]any{
		{"provider_id": uuid.NewString(), "configuration": configuration, "credential_file": secretFile},
		{"provider_id": uuid.NewString(), "configuration": configuration},
	} {
		data, _ := json.Marshal(map[string]any{"providers": []any{entry}})
		file := filepath.Join(directory, "connectors.json")
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadMounted(file, &egress.Policy{}); err == nil || !strings.Contains(err.Error(), "grant") {
			t.Fatalf("mounted a provider that authenticates with a grant: %v", err)
		}
	}
}
