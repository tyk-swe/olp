package runtime

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
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
