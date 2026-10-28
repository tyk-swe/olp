package providers

import (
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestWebRTCCapabilityIsOpenAIOnly(t *testing.T) {
	tuple := CapabilityInput{Operation: "realtime", Surface: "openai", Mode: "webrtc"}
	if _, err := ValidCapabilities([]CapabilityInput{tuple}); err != nil {
		t.Fatalf("webrtc tuple not declarable: %v", err)
	}
	if !certifiable(KindOpenAI, "openai", tuple) {
		t.Fatal("openai webrtc tuple is not certifiable")
	}
	if certifiable(KindAzure, "azure", tuple) {
		t.Fatal("azure webrtc certifiable on openai evidence")
	}
	if !certifiable(KindAzure, "azure", CapabilityInput{Operation: "realtime", Surface: "openai", Mode: "realtime"}) {
		t.Fatal("azure websocket realtime regressed")
	}
	azure := Configuration{Kind: KindAzure, AuthMode: AuthAPIKey, Endpoint: new("https://example.openai.azure.com")}
	azure.Normalize()
	if err := New(nil, &egress.Policy{}, nil).certifyTuple(t.Context(), &azure, []byte("secret"), "model", tuple, 4096); err == nil {
		t.Fatal("azure webrtc certified")
	}
	found := false
	for _, option := range capabilitiesFor(KindOpenAI, "openai") {
		if option == tuple {
			found = true
		}
	}
	if !found {
		t.Fatal("openai capability options omit webrtc")
	}
	for _, option := range capabilitiesFor(KindAzure, "azure") {
		if option == tuple {
			t.Fatal("azure capability options advertise webrtc")
		}
	}
}
