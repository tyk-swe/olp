package geminilifecycle

import (
	"bytes"
	"strings"
	"testing"
)

func TestInteractionsKeepNativeControlsAndStepOrder(t *testing.T) {
	source := []byte(`{"model":"team","input":[{"type":"user_input","content":[{"type":"text","text":"hello"}]}],"generation_config":{"max_output_tokens":64,"seed":0,"future":9007199254740993},"tools":[],"system_instruction":null,"previous_interaction_id":"interaction_parent"}`)
	r, err := ParseInteractionRequest(source, len(source))
	if err != nil || !r.Store || r.Stream || r.Previous != "interaction_parent" {
		t.Fatalf("parse %+v: %v", r, err)
	}
	bound, err := r.Bind("upstream-model", "v1_parent")
	if err != nil || !bytes.Contains(bound, []byte(`"future":9007199254740993`)) || !bytes.Contains(bound, []byte(`"system_instruction":null`)) || !bytes.Contains(bound, []byte(`"previous_interaction_id":"v1_parent"`)) || !bytes.Equal(source, r.Source.Bytes()) {
		t.Fatalf("identity overlay damaged native source: %s %v", bound, err)
	}
	response := []byte(`{"id":"v1_native","status":"completed","steps":[{"type":"thought","signature":"opaque","content":[{"type":"text","text":"reasoning"}]},{"type":"model_output","content":[{"type":"text","text":"OK"}]}],"usage":{"total_tokens":9007199254740993}}`)
	result, err := ParseInteractionResponse(response, len(response))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := result.WithID("interaction_local")
	if err != nil || !bytes.Contains(mapped, []byte(`"signature":"opaque"`)) || !bytes.Contains(mapped, []byte(`"total_tokens":9007199254740993`)) || !bytes.Equal(response, result.Source.Bytes()) {
		t.Fatalf("result overlay damaged steps: %s %v", mapped, err)
	}
	stream := &InteractionStream{}
	for _, data := range []string{
		`{"event_type":"interaction.created","interaction":{"id":"v1_native","status":"in_progress"}}`,
		`{"event_type":"step.start","step":{"type":"thought"}}`,
		`{"event_type":"step.delta","delta":{"type":"text","text":"native"}}`,
		`{"event_type":"step.stop","step":{"type":"thought","signature":"opaque"}}`,
		`{"event_type":"interaction.completed","interaction":{"id":"v1_native","status":"completed","usage":{"total_tokens":4}}}`,
	} {
		event, err := ParseInteractionEvent([]byte(data), len(data))
		if err == nil {
			err = stream.Accept(event)
		}
		if err != nil {
			t.Fatalf("event %s: %v", data, err)
		}
		mapped, err := event.WithID("interaction_local")
		if err != nil || strings.Contains(string(mapped), `"id":"v1_native"`) {
			t.Fatalf("event identity overlay: %s %v", mapped, err)
		}
	}
	if !stream.Done || stream.Step || stream.ID != "v1_native" {
		t.Fatalf("stream state %+v", stream)
	}
}

func TestInteractionsRejectStateAndEventAmbiguity(t *testing.T) {
	for _, raw := range []string{
		`{"model":"m","input":null}`,
		`{"model":"m","input":"x","store":false,"previous_interaction_id":"v1_parent"}`,
		`{"model":"m","input":"x","store":false,"background":true}`,
		`{"model":"m","input":"x","store":null}`,
		`{"model":"m","input":"x","model":"m2"}`,
	} {
		if _, err := ParseInteractionRequest([]byte(raw), 1<<20); err == nil {
			t.Fatalf("accepted malformed interaction %s", raw)
		}
	}
	s := &InteractionStream{}
	event, err := ParseInteractionEvent([]byte(`{"event_type":"step.delta","delta":{"type":"text","text":"x"}}`), 1024)
	if err != nil || s.Accept(event) == nil {
		t.Fatal("accepted a delta before interaction creation")
	}
}

func TestLiveSetupAndFramesKeepNativeAudioAndRejectRenegotiation(t *testing.T) {
	for _, raw := range []string{
		`{"setup":{"model":"models/team","generationConfig":{"responseModalities":["AUDIO"]},"sessionResumption":{},"realtimeInputConfig":{"automaticActivityDetection":{"disabled":true}}}}`,
		`{"setup":{"model":"models/team","generationConfig":{"responseModalities":["AUDIO"]},"sessionResumption":{},"realtimeInputConfig":{"automatic_activity_detection":{"disabled":true}}}}`,
	} {
		setup, err := ParseLiveSetup([]byte(raw), 1<<20)
		if err != nil || setup.Model != "team" {
			t.Fatalf("setup %+v: %v", setup, err)
		}
		bound, err := setup.BindModel("wire-model")
		if err != nil || !bytes.Contains(bound, []byte(`"model":"models/wire-model"`)) || !bytes.Contains(bound, []byte(`"disabled":true`)) {
			t.Fatalf("setup overlay %s: %v", bound, err)
		}
	}
	for _, raw := range []string{
		`{"realtimeInput":{"activityStart":{}}}`,
		`{"realtime_input":{"audio":{"mimeType":"audio/pcm;rate=16000","data":"AQIDBA=="}}}`,
		`{"toolResponse":{"functionResponses":[{"id":"call","response":{"ok":true}}]}}`,
		`{"clientContent":{"turns":[{"role":"user","parts":[{"text":"hello"}]}],"turnComplete":true}}`,
	} {
		if err := ValidateLiveClientFrame([]byte(raw), 1<<20); err != nil {
			t.Fatalf("rejected native client frame %s: %v", raw, err)
		}
	}
	if err := ValidateLiveServerFrame([]byte(`{"serverContent":{"modelTurn":{"parts":[{"inlineData":{"mimeType":"audio/pcm;rate=24000","data":"AQIDBA=="}}]},"interrupted":true,"turnComplete":true},"usageMetadata":{"promptTokenCount":1}}`), 1<<20); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"setup":{"model":"models/team","generationConfig":{},"generation_config":{}}}`,
		`{"setup":{"model":"models/team","sessionResumption":{"handle":"opaque"}}}`,
		`{"setup":{"model":"models/team"},"realtimeInput":{"audio":{}}}`,
	} {
		if _, err := ParseLiveSetup([]byte(raw), 1<<20); err == nil {
			t.Fatalf("accepted malformed setup %s", raw)
		}
	}
	if err := ValidateLiveClientFrame([]byte(`{"setup":{"model":"models/team"}}`), 1024); err == nil {
		t.Fatal("accepted setup renegotiation")
	}
	if err := ValidateLiveServerFrame([]byte(`{"setupComplete":{},"serverContent":{}}`), 1024); err == nil {
		t.Fatal("accepted two principal server events")
	}
	for _, raw := range []string{
		`{"setupComplete":null}`,
		`{"setupComplete":false}`,
		`{"setupComplete":"ready"}`,
		`{"serverContent":[],"usageMetadata":{}}`,
		`{"setupComplete":{},"usageMetadata":null}`,
		`{"setupComplete":{},"futurePrincipal":{"effect":true}}`,
	} {
		if err := ValidateLiveServerFrame([]byte(raw), 1024); err == nil {
			t.Fatalf("accepted malformed Live server frame %s", raw)
		}
	}
	if err := ValidateLiveServerFrame([]byte(`{"setupComplete":{"futureNative":{"ordered":[false,null,9007199254740993]}}}`), 1024); err != nil {
		t.Fatalf("valid native setup extension was removed: %v", err)
	}
}
