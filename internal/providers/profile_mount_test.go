package providers

import (
	"encoding/json"
	"os"
	"path/filepath"
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
