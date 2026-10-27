package duplexcontract_test

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/duplexcontract"
)

func vertexConfig() duplexcontract.Config {
	return duplexcontract.Config{
		Provider: connectors.Config{
			Kind: "vertex_ai", VendorID: "google-vertex", AuthMode: "service_account",
			Endpoint:     connectors.DefaultEndpoint("vertex_ai", "us-central1", "example-project"),
			CloudRegion:  "us-central1",
			CloudProject: "example-project",
			ProfileID:    "vertex-realtime", ProfileRevision: connectors.ProfileRevision,
		},
		Model: "gemini-live-example",
	}
}

func TestCompileAdmitsVertexRealtimeProfile(t *testing.T) {
	template, err := duplexcontract.Compile(vertexConfig())
	if err != nil {
		t.Fatal(err)
	}
	if template.Dialect() != duplexcontract.VertexRealtime || template.Transport() != duplexcontract.TransportWebSocket || !template.SetupAck() {
		t.Fatalf("template = %s/%s ack=%v", template.Dialect(), template.Transport(), template.SetupAck())
	}
	if template.TranscriptionOnly() {
		t.Fatal("Vertex Live must not declare transcription-only sessions")
	}
	wrongVendor := vertexConfig()
	wrongVendor.Provider.VendorID = "google"
	if _, err := duplexcontract.Compile(wrongVendor); err == nil {
		t.Fatal("compile admitted the Gemini vendor under Vertex hosting")
	}
}

func TestVertexDuplexAddressing(t *testing.T) {
	cfg := vertexConfig().Provider
	endpoint, err := cfg.DuplexURL("gemini-live-example")
	if err != nil {
		t.Fatal(err)
	}
	want := "wss://us-central1-aiplatform.googleapis.com/ws/google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent"
	if endpoint != want {
		t.Fatalf("endpoint = %s, want %s", endpoint, want)
	}
	bound, err := cfg.DuplexBoundModel("gemini-live-example")
	if err != nil {
		t.Fatal(err)
	}
	if bound != "projects/example-project/locations/us-central1/publishers/google/models/gemini-live-example" {
		t.Fatalf("bound model = %s", bound)
	}
}

func TestVertexSetupInspectionAndBinding(t *testing.T) {
	template, err := duplexcontract.Compile(vertexConfig())
	if err != nil {
		t.Fatal(err)
	}
	setup, err := template.InspectSetup([]byte(`{"setup":{"model":"models/team-live","generationConfig":{"responseModalities":["AUDIO"]}}}`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if setup.Model != "team-live" || setup.TranscriptionOnly {
		t.Fatalf("setup = %+v", setup)
	}
	full, err := template.InspectSetup([]byte(`{"setup":{"model":"projects/example-project/locations/us-central1/publishers/google/models/team-live"}}`), 1<<20)
	if err != nil || full.Model != "team-live" {
		t.Fatalf("full resource setup = %+v err=%v", full, err)
	}
	if _, err := template.InspectSetup([]byte(`{"setup":{"model":"models/team-live","sessionResumption":{"handle":"foreign-handle"}}}`), 1<<20); err == nil {
		t.Fatal("a provider session resumption handle was admitted")
	}
	if _, err := template.InspectSetup([]byte(`{"setup":{"model":"models/team-live","inputAudioTranscription":{},"generationConfig":{"responseModalities":["AUDIO"]}}}`), 1<<20); err == nil {
		t.Fatal("mixed input transcription without a complete meter was admitted")
	}
	if _, err := template.InspectSetup([]byte(`{"setup":{"model":"models/team-live","output_audio_transcription":{},"generationConfig":{"responseModalities":["AUDIO"]}}}`), 1<<20); err == nil {
		t.Fatal("mixed output transcription without a complete meter was admitted")
	}
	transcription, err := template.InspectSetup([]byte(`{"setup":{"model":"models/team-live","inputAudioTranscription":{},"generationConfig":{"responseModalities":[]}}}`), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !transcription.TranscriptionOnly || template.AdmitSetup(transcription) == nil {
		t.Fatal("transcription-only Vertex setup was not gated by the profile declaration")
	}
	bound, err := template.BindSetup([]byte(`{"setup":{"model":"models/team-live"}}`), "projects/example-project/locations/us-central1/publishers/google/models/gemini-live-example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bound), `"projects/example-project/locations/us-central1/publishers/google/models/gemini-live-example"`) {
		t.Fatalf("bound setup = %s", bound)
	}
}

func TestVertexObserverAccumulatesTurnUsage(t *testing.T) {
	template, err := duplexcontract.Compile(vertexConfig())
	if err != nil {
		t.Fatal(err)
	}
	observer := template.Observer(1 << 20)
	ack, err := observer.ServerFrame([]byte(`{"setupComplete":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ack.SetupComplete {
		t.Fatal("setupComplete frame was not classified")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":9,"responseTokenCount":5,"totalTokenCount":14}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"thoughtsTokenCount":3,"toolUsePromptTokenCount":1,"totalTokenCount":9,"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":2}]}}`)); err != nil {
		t.Fatal(err)
	}
	usage := observer.Usage()
	if usage == nil || usage.InputTokens != 13 || usage.OutputTokens != 10 || usage.TotalTokens != 23 {
		t.Fatalf("usage = %+v, want Vertex candidates usage with tool and thought tokens folded in", usage)
	}
	if usage.AudioOutputTokens == nil || *usage.AudioOutputTokens != 2 || usage.ReasoningTokens == nil || *usage.ReasoningTokens != 3 {
		t.Fatalf("usage shares = %+v", usage)
	}
	if !observer.Complete() {
		t.Fatal("completed turns did not produce complete usage evidence")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("session settled as complete after a turn ended without usage")
	}
	if _, err := observer.ServerFrame([]byte(`{"usageMetadata":{"promptTokenCount":4,"responseTokenCount":1,"totalTokenCount":5}}`)); err != nil {
		t.Fatalf("standalone usage message refused: %v", err)
	}
	usage = observer.Usage()
	if usage == nil || usage.InputTokens != 17 || usage.OutputTokens != 11 || usage.TotalTokens != 28 {
		t.Fatalf("standalone usage not applied to the finished turn: %+v", usage)
	}
	if !observer.Complete() {
		t.Fatal("standalone terminal usage did not complete the session")
	}
	if err := observer.ClientFrame([]byte(`{"clientContent":{"turns":[{"role":"user","parts":[{"text":"again"}]}],"turnComplete":true}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("session settled as complete after submitting a turn that never started")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
		t.Fatal(err)
	}
	if !observer.Complete() {
		t.Fatal("submitted turn settled by terminal usage is not complete")
	}
	if err := observer.ClientFrame([]byte(`{"realtimeInput":{"activityEnd":{}}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("session settled as complete after ending realtime input that never started")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
		t.Fatal(err)
	}
	if !observer.Complete() {
		t.Fatal("realtime turn settled by terminal usage is not complete")
	}
	if err := observer.ClientFrame([]byte(`{"client_content":{"turns":[],"turn_complete":true}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("session settled as complete after a snake_case submission that never started")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
		t.Fatal(err)
	}
	if !observer.Complete() {
		t.Fatal("snake_case submission settled by terminal usage is not complete")
	}
	if err := observer.ClientFrame([]byte(`{"realtime_input":{"activity_end":{}}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("session settled as complete after ending snake_case realtime input that never started")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
		t.Fatal(err)
	}
	if !observer.Complete() {
		t.Fatal("snake_case realtime turn settled by terminal usage is not complete")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"outputTranscription":{"text":"hello"}}}`)); err != nil {
		t.Fatal(err)
	}
	if !observer.Complete() {
		t.Fatal("trailing transcription reopened a settled session")
	}
	if err := observer.ClientFrame([]byte(`{"realtimeInput":{"audio":{"mimeType":"audio/pcm;rate=16000","data":"AQIDBA=="}}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("session settled as complete after streaming audio that never started")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
		t.Fatal(err)
	}
	if !observer.Complete() {
		t.Fatal("streamed audio settled by terminal usage is not complete")
	}
	if err := observer.ClientFrame([]byte(`{"clientContent":{"turns":[{"role":"user","parts":[{"text":"again"}]}],"turnComplete":true}}`)); err != nil {
		t.Fatal(err)
	}
	for _, frame := range []string{
		`{"serverContent":{"turnComplete":true}}`,
		`{"sessionResumptionUpdate":{"resumable":true}}`,
		`{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`,
	} {
		if _, err := observer.ServerFrame([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if !observer.Complete() {
		t.Fatal("control frame between turn completion and standalone usage broke settlement")
	}
	for _, frame := range []string{
		`{"toolResponse":{"functionResponses":[{"id":"call_1","response":{"result":"ok"}}]}}`,
		`{"tool_response":{"function_responses":[{"id":"call_2","response":{"result":"ok"}}]}}`,
	} {
		if err := observer.ClientFrame([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		if observer.Complete() {
			t.Fatal("session settled as complete after a tool response that never started")
		}
		if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
			t.Fatal(err)
		}
		if !observer.Complete() {
			t.Fatal("tool response settled by terminal usage is not complete")
		}
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true}}`)); err != nil {
		t.Fatal(err)
	}
	if err := observer.ClientFrame([]byte(`{"realtimeInput":{"audio":{"mimeType":"audio/pcm;rate=16000","data":"AQIDBA=="}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.ServerFrame([]byte(`{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("delayed usage for the finished turn covered newer realtime input")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
		t.Fatal(err)
	}
	if !observer.Complete() {
		t.Fatal("newer realtime input settled by its own terminal usage is not complete")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true}}`)); err != nil {
		t.Fatal(err)
	}
	if err := observer.ClientFrame([]byte(`{"clientContent":{"turns":[{"role":"user","parts":[{"text":"next"}]}],"turnComplete":true}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.ServerFrame([]byte(`{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("delayed usage for the finished turn discharged a newer text submission")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}`)); err != nil {
		t.Fatal(err)
	}
	if !observer.Complete() {
		t.Fatal("newer text submission settled by its own terminal usage is not complete")
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"turnComplete":true}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.ServerFrame([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"next"}]}}}`)); err != nil {
		t.Fatal(err)
	}
	if observer.Complete() {
		t.Fatal("session settled as complete after a later turn produced no usage at all")
	}
	if err := observer.ClientFrame([]byte(`{"setup":{"model":"models/team-live"}}`)); err == nil {
		t.Fatal("a second setup frame was admitted mid-session")
	}
	if _, err := observer.ServerFrame([]byte(`{"unknownEvent":{}}`)); err == nil {
		t.Fatal("unregistered provider event was admitted")
	}
}
