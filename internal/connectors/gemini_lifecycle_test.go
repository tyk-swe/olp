package connectors

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestGeminiLifecycleProfilesHaveDistinctAddressAndCapability(t *testing.T) {
	interactions := Config{Kind: "gemini", AuthMode: "api_key", Endpoint: "https://provider.example/v1beta", ProfileID: "gemini-interactions", ProfileRevision: ProfileRevision}
	live := Config{Kind: "gemini", AuthMode: "api_key", Endpoint: "https://provider.example/v1beta", ProfileID: "gemini-live", ProfileRevision: ProfileRevision}
	for _, cfg := range []Config{interactions, live} {
		if err := cfg.Validate(&egress.Policy{}); err != nil {
			t.Fatalf("%s: %v", cfg.ProfileID, err)
		}
	}
	if got, err := interactions.InteractionsURL("", ""); err != nil || got != "https://provider.example/v1beta/interactions" {
		t.Fatalf("create address %s: %v", got, err)
	}
	if got, err := interactions.InteractionsURL("v1_resource", "cancel"); err != nil || got != "https://provider.example/v1beta/interactions/v1_resource/cancel" {
		t.Fatalf("cancel address %s: %v", got, err)
	}
	if got, err := live.GeminiLiveURL(); err != nil || got != "wss://provider.example/ws/"+GeminiLiveMethod {
		t.Fatalf("Live address %s: %v", got, err)
	}
	if !interactions.Supports("generation", "gemini", "unary") || !interactions.Supports("generation", "gemini", "streaming") || interactions.Supports("realtime", "gemini", "realtime") || interactions.Supports("generation", "openai", "unary") {
		t.Fatal("Interactions capability fell through another dialect")
	}
	if !live.Supports("realtime", "gemini", "realtime") || live.Supports("generation", "gemini", "unary") || live.Supports("realtime", "openai", "realtime") {
		t.Fatal("Live capability fell through another dialect")
	}
	if _, err := live.InteractionsURL("", ""); err == nil {
		t.Fatal("Live profile could address Interactions")
	}
	if _, err := interactions.GeminiLiveURL(); err == nil {
		t.Fatal("Interactions profile could address Live")
	}
	if _, err := interactions.InteractionsURL("../other", ""); err == nil {
		t.Fatal("resource ID could change the target path")
	}
	if _, err := interactions.TargetFamily(openai.FamilyGemini); err == nil {
		t.Fatal("Interactions profile silently fell through GenerateContent")
	}
	withDefaults := interactions
	withDefaults.OperationDefaults = map[string]DefaultSet{"generation": {Dialect: "gemini-interactions", NativeOptions: map[string]json.RawMessage{"tools": json.RawMessage(`[]`)}}}
	if err := withDefaults.ValidateProfile(); err == nil {
		t.Fatal("unimplemented lifecycle defaults would be silently ignored")
	}
}
