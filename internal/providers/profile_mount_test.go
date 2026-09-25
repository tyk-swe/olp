package providers

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
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

// Published snapshots key providers by canonical UUID, so another spelling
// could never match and could hide a duplicate entry.
func TestMountedProviderIDsAreCanonical(t *testing.T) {
	const id = "abcdef01-2345-4678-9abc-def012345678"
	entry := func(providerID string) any {
		return map[string]any{"provider_id": providerID, "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "none", "endpoint": "https://upstream.example/v1"}}
	}
	for name, providers := range map[string][]any{
		"uppercase": {entry(strings.ToUpper(id))},
		"braced":    {entry("{" + id + "}")},
		"urn":       {entry("urn:uuid:" + id)},
		"duplicate": {entry(id), entry(strings.ToUpper(id))},
	} {
		data, _ := json.Marshal(map[string]any{"providers": providers})
		file := filepath.Join(t.TempDir(), "connectors.json")
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
		if entries, err := LoadMounted(file, &egress.Policy{}); err == nil {
			t.Errorf("%s: accepted non-canonical provider identifiers: %v", name, slices.Collect(maps.Keys(entries)))
		}
	}
}
