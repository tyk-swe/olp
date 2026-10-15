package runtime

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestMountedTransportPreservesPublishedOwnership(t *testing.T) {
	id := uuid.NewString()
	s := routing.Snapshot{Providers: map[string]routing.Provider{id: {ID: id, Enabled: true, Kind: "openai"}}}
	slotID, secretID := uuid.NewString(), uuid.NewString()
	p := s.Providers[id]
	p.AuthMode = "api_key"
	p.Endpoint = "https://published.example/v1"
	p.DefaultSlotID = slotID
	p.Slots = []routing.Slot{{ID: slotID, Name: "default", Enabled: true, Weight: 1, CredentialID: &secretID, RequestsPerMinute: ptr(int64(3))}}
	s.Providers = map[string]routing.Provider{id: p}
	mounted := map[string]MountedProvider{id: {Configuration: routing.Configuration{Kind: "openai", AuthMode: "api_key", Endpoint: "https://mounted.example/v1"}, Credential: []byte("mounted-secret")}}
	secrets, e := installMounted(&s, mounted)
	if e != nil {
		t.Fatal(e)
	}
	got := s.Providers[id]
	if got.Endpoint != "https://mounted.example/v1" || *got.Slots[0].RequestsPerMinute != 3 || string(secrets[secretID]) != "mounted-secret" {
		t.Fatal("mount changed published authority")
	}
	missingIdentity := got
	missingIdentity.DefaultSlotID = ""
	s.Providers[id] = missingIdentity
	if _, err := installMounted(&s, mounted); err == nil {
		t.Fatal("a mutable slot name became authoritative default-slot identity")
	}
	got.Slots = append(got.Slots, routing.Slot{ID: uuid.NewString(), Name: "named", Enabled: true})
	s.Providers[id] = got
	if _, e = installMounted(&s, mounted); e == nil {
		t.Fatal("mounted credential synthesized a named pool")
	}
}
