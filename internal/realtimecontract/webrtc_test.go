package realtimecontract_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/realtimecontract"
)

func TestCompileWebRTCAdmitsOnlyOpenAIDirect(t *testing.T) {
	template, err := realtimecontract.CompileWebRTC(realtimeConfig("openai-responses", "openai", "https://api.openai.example/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if !template.WebRTC() || template.Model() != "native-model" {
		t.Fatalf("template = %+v", template)
	}
	for _, tc := range []struct{ profile, kind, endpoint string }{
		{"azure-v1-responses", "azure_openai", "https://resource.example"},
		{"compatible-responses", "openai_compatible", "https://compat.example/v1"},
		{"openai-responses", "openai_compatible", "https://compat.example/v1"},
	} {
		if _, err := realtimecontract.CompileWebRTC(realtimeConfig(tc.profile, tc.kind, tc.endpoint)); err == nil {
			t.Fatalf("admitted %s/%s", tc.profile, tc.kind)
		}
	}
}

func TestCompileWebRTCRefusesUnenforceablePolicy(t *testing.T) {
	cfg := realtimeConfig("openai-responses", "openai", "https://api.openai.example/v1")
	cfg.Policy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{Pattern: "x", Action: "block"}}}
	if _, err := realtimecontract.CompileWebRTC(cfg); err == nil {
		t.Fatal("admitted content policy the control channel cannot enforce")
	}
	ws, err := realtimecontract.Compile(realtimeConfig("openai-responses", "openai", "https://api.openai.example/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if ws.WebRTC() {
		t.Fatal("websocket template reports WebRTC")
	}
}
