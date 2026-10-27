package connectors_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
)

func TestWebRTCSupportsIsDistinctFromWebSocketRealtime(t *testing.T) {
	if !connectors.Supports("openai", "openai", "realtime", "openai", "webrtc") {
		t.Fatal("openai webrtc tuple unsupported")
	}
	if connectors.Supports("azure_openai", "azure", "realtime", "openai", "webrtc") {
		t.Fatal("azure webrtc admitted on openai evidence")
	}
	if !connectors.Supports("azure_openai", "azure", "realtime", "openai", "realtime") {
		t.Fatal("azure websocket realtime regressed")
	}
	if connectors.Supports("openai", "openai", "generation", "openai", "webrtc") {
		t.Fatal("webrtc mode leaked to generation")
	}
	cfg := connectors.Config{Kind: "openai", VendorID: "openai", ProfileID: "openai-responses", ProfileRevision: connectors.ProfileRevision, AuthMode: "api_key", Endpoint: "https://api.openai.example/v1"}
	if !cfg.Supports("realtime", "openai", "webrtc") {
		t.Fatal("openai profile does not support webrtc")
	}
	azure := connectors.Config{Kind: "azure_openai", VendorID: "azure", ProfileID: "azure-v1-responses", ProfileRevision: connectors.ProfileRevision, AuthMode: "api_key", Endpoint: "https://resource.example"}
	if azure.Supports("realtime", "openai", "webrtc") {
		t.Fatal("azure profile supports webrtc")
	}
}

func TestRealtimeCallsURLs(t *testing.T) {
	cfg := connectors.Config{Kind: "openai", VendorID: "openai", ProfileID: "openai-responses", ProfileRevision: connectors.ProfileRevision, AuthMode: "api_key", Endpoint: "https://api.openai.example/v1"}
	calls, err := cfg.RealtimeCallsURL("native-model")
	if err != nil || calls != "https://api.openai.example/v1/realtime/calls" {
		t.Fatalf("calls=%q err=%v", calls, err)
	}
	control, err := cfg.RealtimeControlURL("native-model", "rtc_123")
	if err != nil || control != "wss://api.openai.example/v1/realtime?call_id=rtc_123" {
		t.Fatalf("control=%q err=%v", control, err)
	}
	hangup, err := cfg.RealtimeHangupURL("native-model", "rtc_123")
	if err != nil || hangup != "https://api.openai.example/v1/realtime/calls/rtc_123/hangup" {
		t.Fatalf("hangup=%q err=%v", hangup, err)
	}
	for _, bad := range []string{"", "rtc 1", "../x", "a/b", "a?b"} {
		if _, err := cfg.RealtimeControlURL("native-model", bad); err == nil {
			t.Fatalf("admitted call id %q", bad)
		}
		if _, err := cfg.RealtimeHangupURL("native-model", bad); err == nil {
			t.Fatalf("admitted hangup call id %q", bad)
		}
	}
}
