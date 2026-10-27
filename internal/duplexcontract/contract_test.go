package duplexcontract_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/duplexcontract"
)

func xaiConfig() duplexcontract.Config {
	return duplexcontract.Config{
		Provider: connectors.Config{
			Kind: "openai_compatible", VendorID: "xai", AuthMode: "api_key",
			Endpoint: "https://api.x.ai/v1", ProfileID: "xai-realtime", ProfileRevision: connectors.ProfileRevision,
		},
		Model: "grok-voice",
	}
}

func TestCompileAdmitsXAIRealtimeProfile(t *testing.T) {
	template, err := duplexcontract.Compile(xaiConfig())
	if err != nil {
		t.Fatal(err)
	}
	if template.Dialect() != duplexcontract.XAIRealtime || template.ProfileID() != "xai-realtime" {
		t.Fatalf("template identity = %s/%s", template.Dialect(), template.ProfileID())
	}
	if template.TranscriptionOnly() {
		t.Fatal("xAI realtime must not declare transcription-only sessions")
	}
	if template.Transport() != duplexcontract.TransportWebSocket {
		t.Fatalf("transport = %s", template.Transport())
	}
}

func TestCompileRefusesMismatchedVendorAndPolicy(t *testing.T) {
	wrongVendor := xaiConfig()
	wrongVendor.Provider.VendorID = "openai"
	if _, err := duplexcontract.Compile(wrongVendor); err == nil {
		t.Fatal("compile admitted a vendor the profile does not review")
	}
	policied := xaiConfig()
	policied.Policy = &contentpolicy.Policy{Rules: []contentpolicy.Rule{{ID: "r1"}}}
	if _, err := duplexcontract.Compile(policied); err == nil {
		t.Fatal("compile admitted an inspection policy opaque duplex frames cannot enforce")
	}
	otherProfile := xaiConfig()
	otherProfile.Provider.ProfileID, otherProfile.Provider.Kind = "compatible-chat", "openai_compatible"
	if _, err := duplexcontract.Compile(otherProfile); err == nil {
		t.Fatal("compile admitted a non-duplex profile")
	}
}

func TestXAISetupTranscriptionGating(t *testing.T) {
	template, err := duplexcontract.Compile(xaiConfig())
	if err != nil {
		t.Fatal(err)
	}
	setup, err := template.InspectSetup([]byte(`{"type":"session.update","session":{"type":"transcription","audio":{"input":{"transcription":{"model":"whisper"}}}}}`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !setup.TranscriptionOnly || template.AdmitSetup(setup) == nil {
		t.Fatal("transcription-only session was not gated by the profile declaration")
	}
	voice, err := template.InspectSetup([]byte(`{"type":"session.update","session":{"type":"realtime","voice":"eve"}}`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if voice.TranscriptionOnly || template.AdmitSetup(voice) != nil {
		t.Fatal("ordinary voice session was refused as transcription-only")
	}
	if _, err := template.InspectSetup([]byte(`{"type":`), 1<<20); err == nil {
		t.Fatal("malformed setup frame was admitted")
	}
}

func TestXAIObserverExtractsTerminalUsage(t *testing.T) {
	template, err := duplexcontract.Compile(xaiConfig())
	if err != nil {
		t.Fatal(err)
	}
	observer := template.Observer(1 << 20)
	frame, err := observer.ServerFrame([]byte(`{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`))
	if err != nil {
		t.Fatal(err)
	}
	usage := observer.Usage()
	if usage == nil || usage.InputTokens != 11 || usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", usage)
	}
	if frame.ProviderError {
		t.Fatal("usage frame reported as provider error")
	}
	second, err := observer.ServerFrame([]byte(`{"type":"error","error":{"message":"boom"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !second.ProviderError {
		t.Fatal("error frame was not classified as provider error")
	}
	if _, err := observer.ServerFrame([]byte(`{bad json`)); err == nil {
		t.Fatal("malformed provider frame was admitted")
	}
}

func TestXAIObserverPendingResponseKeepsSessionIncomplete(t *testing.T) {
	template, err := duplexcontract.Compile(xaiConfig())
	if err != nil {
		t.Fatal(err)
	}
	observer := template.Observer(1 << 20)
	for _, frame := range []string{
		`{"type":"response.created","response":{"id":"r1"}}`,
		`{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
	} {
		if _, err := observer.ServerFrame([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if !observer.Complete() {
		t.Fatal("session with every response settled by terminal usage is not complete")
	}
	if _, err := observer.ServerFrame([]byte(`{"type":"response.created","response":{"id":"r2"}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("session settled as complete while a later response has no terminal usage")
	}
	if _, err := observer.ServerFrame([]byte(`{"type":"response.done","response":{"id":"r2"}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("session settled as complete after a response ended without usage")
	}
}
