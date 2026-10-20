package connectors

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/egress"
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
	if _, err := interactions.TargetFamily(wire.Gemini); err == nil {
		t.Fatal("Interactions profile silently fell through GenerateContent")
	}
	automatic := Config{Kind: "gemini", AuthMode: "api_key", Endpoint: "https://provider.example/v1beta"}
	generation := Config{Kind: "gemini", AuthMode: "api_key", Endpoint: "https://provider.example/v1beta", ProfileID: "gemini-generation", ProfileRevision: ProfileRevision}
	if !automatic.Supports("batch", "gemini", "unary") || !generation.Supports("batch", "gemini", "unary") {
		t.Fatal("direct Gemini connector did not gain native batch support")
	}
	if !automatic.SupportsFilesBatches() || !generation.SupportsFilesBatches() ||
		!automatic.Supports("batch", "openai", "unary") || !generation.Supports("batch", "openai", "unary") ||
		automatic.FilesBatchesAdapter() != "gemini_managed" || generation.FilesBatchesAdapter() != "gemini_managed" {
		t.Fatal("direct Gemini connector did not gain managed OpenAI files/batches support")
	}
	profile, err := generation.profile()
	if err != nil || !slices.Contains(profile.Operations, "batch") {
		t.Fatal("Gemini GenerateContent profile does not list batch")
	}
	if interactions.Supports("batch", "gemini", "unary") || live.Supports("batch", "gemini", "unary") {
		t.Fatal("Gemini lifecycle profiles gained file/batch support")
	}
	vertex := Config{Kind: "vertex_ai", AuthMode: "adc", Endpoint: "https://us-central1-aiplatform.googleapis.com/v1", ProfileID: "vertex-gemini", ProfileRevision: ProfileRevision}
	if vertex.Supports("batch", "gemini", "unary") {
		t.Fatal("Vertex Gemini gained direct Files API support")
	}
	withDefaults := interactions
	withDefaults.OperationDefaults = map[string]DefaultSet{"generation": {Dialect: "gemini-interactions", NativeOptions: map[string]json.RawMessage{"tools": json.RawMessage(`[]`)}}}
	if err := withDefaults.ValidateProfile(); err == nil {
		t.Fatal("unimplemented lifecycle defaults would be silently ignored")
	}
}
