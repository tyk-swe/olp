package media

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

func openAIProfileConfig(op string, values, native map[string]json.RawMessage) connectors.Config {
	defaults := map[string]connectors.DefaultSet{}
	if len(values) > 0 || len(native) > 0 {
		defaults[op] = connectors.DefaultSet{Dialect: "direct-openai/" + op, Values: values, NativeOptions: native}
	}
	return connectors.Config{
		Kind:              connectors.KindOpenAI,
		AuthMode:          connectors.AuthAPIKey,
		ProfileID:         "openai-chat",
		ProfileRevision:   connectors.ProfileRevision,
		OperationDefaults: defaults,
	}
}

func TestEncodeConfiguredJSONDefaultsAndGuards(t *testing.T) {
	req, failure := DecodeImageGeneration([]byte(`{"model":"img-route","prompt":"a boat","size":"1024x1024"}`))
	if failure != nil {
		t.Fatalf("decode failed: %v", failure)
	}
	cfg := openAIProfileConfig("image_generation", map[string]json.RawMessage{
		"size":    json.RawMessage(`"1536x1024"`),
		"quality": json.RawMessage(`"high"`),
	}, nil)
	call, effective, failure := EncodeConfigured(req, cfg, "gpt-image-1")
	if failure != nil {
		t.Fatalf("EncodeConfigured failed: %v", failure)
	}
	fields := mustJSONFields(t, call.Body)
	if string(fields["model"]) != `"gpt-image-1"` || string(fields["size"]) != `"1024x1024"` || string(fields["quality"]) != `"high"` {
		t.Fatalf("unexpected configured image body: %v", fields)
	}
	if p := effective.Params().(ImageGeneration); p.Quality == nil || *p.Quality != "high" {
		t.Fatalf("expected effective quality=high, got %+v", p)
	}

	speechReq, _ := DecodeSpeech([]byte(`{"model":"tts-route","input":"hi","voice":"alloy"}`))
	streamChangeCfg := openAIProfileConfig("speech", map[string]json.RawMessage{
		"stream_format": json.RawMessage(`"sse"`),
	}, nil)
	if _, _, err := EncodeConfigured(speechReq, streamChangeCfg, "gpt-4o-mini-tts"); err == nil || err.Code != "unsupported_parameter" {
		t.Fatalf("expected delivery mode change error, got %v", err)
	}

	vendorCfg := connectors.Config{
		Kind:            connectors.KindOpenAICompatible,
		AuthMode:        connectors.AuthAPIKey,
		VendorID:        "recraft",
		ProfileID:       "compatible-chat",
		ProfileRevision: connectors.ProfileRevision,
		OperationDefaults: map[string]connectors.DefaultSet{
			"image_generation": {Dialect: "direct-compatible/image_generation", Values: map[string]json.RawMessage{"quality": json.RawMessage(`"high"`)}},
		},
	}
	if _, _, err := EncodeConfigured(req, vendorCfg, "recraftv3"); err == nil {
		t.Fatalf("expected vendor translated API to refuse configured defaults")
	}
}

func TestEncodeConfiguredAndStrictMultipart(t *testing.T) {
	audio := testPart("a1", "clip.wav", "audio/wav")
	req, failure := DecodeTranscription(staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("stt-route")},
		{Name: "file", Value: audio},
	}, false))
	if failure != nil {
		t.Fatalf("decode failed: %v", failure)
	}

	cfg := openAIProfileConfig("transcription", map[string]json.RawMessage{
		"language": json.RawMessage(`"en"`),
	}, map[string]json.RawMessage{
		"include": json.RawMessage(`["logprobs"]`),
	})
	call, _, failure := EncodeStrictConfigured(req, cfg, "gpt-4o-transcribe")
	if failure != nil {
		t.Fatalf("EncodeStrictConfigured failed: %v", failure)
	}
	parts := call.Body.(FormBody)
	got := map[string]string{}
	for _, field := range parts {
		if text, ok := field.Value.(staging.Text); ok {
			got[field.Name] = string(text)
		}
	}
	if got["model"] != "gpt-4o-transcribe" || got["language"] != "en" || got["include[]"] != "logprobs" {
		t.Fatalf("unexpected strict multipart fields: %v", got)
	}

	normalizedReq, _ := DecodeTranscription(staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("stt-route")},
		{Name: "file", Value: audio},
	}, true))
	if _, _, err := EncodeStrictConfigured(normalizedReq, openAIProfileConfig("transcription", nil, nil), "gpt-4o-transcribe"); err == nil {
		t.Fatalf("expected strict configured to reject normalized multipart source")
	}

	nullDefaultCfg := openAIProfileConfig("transcription", nil, map[string]json.RawMessage{
		"custom_opt": json.RawMessage(`null`),
	})
	if _, _, err := EncodeConfigured(req, nullDefaultCfg, "gpt-4o-transcribe"); err == nil || !strings.Contains(err.Message, "null") {
		t.Fatalf("expected multipart null default error, got %v", err)
	}

	if err := placeable(OpTranscription, "include", json.RawMessage(`["logprobs"]`), []string{"include[0]"}, false, true); err == nil {
		t.Fatalf("expected multipart member collision error")
	}
}

func TestEncodeRequestJSONCollisionGuard(t *testing.T) {
	_, failure := encodeRequestJSON(speechRequestWire{Model: "m", Input: "hi", Voice: "v"}, map[string]json.RawMessage{
		"voice": json.RawMessage(`"override"`),
	})
	if failure == nil || !strings.Contains(failure.Message, "collides") {
		t.Fatalf("expected collision error, got %v", failure)
	}
}
