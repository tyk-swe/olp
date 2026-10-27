package duplexcontract_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/duplexcontract"
)

func bedrockConfig(t *testing.T) duplexcontract.Config {
	t.Helper()
	endpoint, err := connectors.BedrockEndpoint("us-east-1", false)
	if err != nil {
		t.Fatal(err)
	}
	return duplexcontract.Config{
		Provider: connectors.Config{
			Kind: "bedrock", VendorID: "amazon-bedrock", AuthMode: "static",
			Endpoint:    endpoint,
			CloudRegion: "us-east-1",
			ProfileID:   "bedrock-realtime", ProfileRevision: connectors.ProfileRevision,
		},
		Model: "amazon.nova-sonic-v1:0",
	}
}

func TestCompileAdmitsBedrockRealtimeProfile(t *testing.T) {
	template, err := duplexcontract.Compile(bedrockConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if template.Dialect() != duplexcontract.BedrockRealtime || template.Transport() != duplexcontract.TransportBedrockBidi {
		t.Fatalf("template = %s/%s", template.Dialect(), template.Transport())
	}
	if template.TranscriptionOnly() || template.SetupAck() {
		t.Fatal("Nova Sonic declares neither transcription-only sessions nor a setup acknowledgement")
	}
	wrongVendor := bedrockConfig(t)
	wrongVendor.Provider.VendorID = "amazon-sagemaker"
	if _, err := duplexcontract.Compile(wrongVendor); err == nil {
		t.Fatal("compile admitted a vendor the Bedrock profile does not review")
	}
	noRegion := bedrockConfig(t)
	noRegion.Provider.CloudRegion = ""
	if _, err := duplexcontract.Compile(noRegion); err == nil {
		t.Fatal("compile admitted a Bedrock target with no region to address")
	}
}

func TestBedrockSetupAndFrameGrammar(t *testing.T) {
	template, err := duplexcontract.Compile(bedrockConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	setup, err := template.InspectSetup([]byte(`{"event":{"sessionStart":{"inferenceConfig":{"maxTokens":1024}}}}`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if setup.TranscriptionOnly || template.AdmitSetup(setup) != nil {
		t.Fatalf("setup = %+v", setup)
	}
	if _, err := template.InspectSetup([]byte(`{"event":{"audioInput":{"content":"AA=="}}}`), 1<<20); err == nil {
		t.Fatal("a Nova Sonic session was admitted without sessionStart first")
	}
	observer := template.Observer(1 << 20)
	if err := observer.ClientFrame([]byte(`{"event":{"audioInput":{"content":"AA=="}}}`)); err != nil {
		t.Fatal(err)
	}
	if err := observer.ClientFrame([]byte(`{"event":{"teleport":{}}}`)); err == nil {
		t.Fatal("unregistered client event was admitted")
	}
	if _, err := observer.ServerFrame([]byte(`{"event":{"textOutput":{"content":"hello"}}}`)); err != nil {
		t.Fatal(err)
	}
	for _, frame := range []string{
		`{"event":{"completionStart":{"sessionId":"s1","promptName":"p1","completionId":"c1"}}}`,
		`{"event":{"usageEvent":{"completionId":"c1","details":{"delta":{"inputTokens":3}}}}}`,
		`{"event":{"completionEnd":{"sessionId":"s1","promptName":"p1","completionId":"c1","stopReason":"END_TURN"}}}`,
	} {
		if _, err := observer.ServerFrame([]byte(frame)); err != nil {
			t.Fatalf("documented Nova Sonic output event refused: %s: %v", frame, err)
		}
	}
	if _, err := observer.ServerFrame([]byte(`{"event":{"mystery":{}}}`)); err == nil {
		t.Fatal("unregistered provider event was admitted")
	}
	if observer.Usage() != nil || observer.Complete() {
		t.Fatal("Nova Sonic produced terminal usage evidence its wire does not carry; settlement must stay uncertain")
	}
}
