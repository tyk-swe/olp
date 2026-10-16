package media

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
)

func mediaDefaultsConfig(t *testing.T, operation string, values, native map[string]json.RawMessage) connectors.Config {
	t.Helper()
	profile, err := connectors.LookupProfile("openai-chat", "1")
	if err != nil {
		t.Fatal(err)
	}
	return connectors.Config{
		Kind: "openai", AuthMode: "api_key", ProfileID: profile.ID, ProfileRevision: profile.Revision,
		OperationDefaults: map[string]connectors.DefaultSet{operation: {Dialect: profile.OperationDialect(operation), Values: values, NativeOptions: native}},
	}
}

func TestConfiguredMultipartArrayDefaultsAreAtomicAndOrdered(t *testing.T) {
	r := &Request{Op: OpTranscription, Route: "route", File: &Part{Handle: "staged-file"}, Format: new("verbose_json"), Temperature: new(float64(0)), Language: new("")}
	cfg := mediaDefaultsConfig(t, OpTranscription, map[string]json.RawMessage{
		"temperature": json.RawMessage(`0.5`), "language": json.RawMessage(`"en"`),
		"timestamp_granularities": json.RawMessage(`["segment","word"]`),
		"chunking_strategy":       json.RawMessage(`{"type":"server_vad","silence_duration_ms":500}`),
	}, nil)
	call, effective, failure := EncodeConfigured(r, cfg, "transcription-model")
	if failure != nil {
		t.Fatal(failure)
	}
	var granularities []string
	values := map[string]string{}
	for _, field := range call.Fields {
		if field.Text == nil {
			continue
		}
		values[field.Name] = *field.Text
		if field.Name == "timestamp_granularities[]" {
			granularities = append(granularities, *field.Text)
		}
	}
	if !reflect.DeepEqual(granularities, []string{"segment", "word"}) || values["temperature"] != "0" || values["language"] != "" || values["chunking_strategy"] != `{"type":"server_vad","silence_duration_ms":500}` {
		t.Fatalf("multipart control shape/order changed: %+v %v", values, granularities)
	}
	if !reflect.DeepEqual(effective.TimestampGranularities, granularities) || r.TimestampGranularities != nil || r.ChunkingStrategy != nil {
		t.Fatal("array defaults did not detach from source")
	}
}

func TestConfiguredMediaRejectsUnrepresentableDefaultsAndCollisions(t *testing.T) {
	for _, test := range []struct {
		name           string
		request        *Request
		values, native map[string]json.RawMessage
	}{
		{"multipart null", &Request{Op: OpTranscription, Route: "route"}, map[string]json.RawMessage{"temperature": json.RawMessage(`null`)}, nil},
		{"empty repeated array", &Request{Op: OpTranscription, Route: "route"}, map[string]json.RawMessage{"timestamp_granularities": json.RawMessage(`[]`)}, nil},
		{"decoder conflict", &Request{Op: OpTranscription, Route: "route", Include: []string{"logprobs"}}, map[string]json.RawMessage{"response_format": json.RawMessage(`"text"`)}, nil},
		{"bad speed", &Request{Op: OpSpeech, Route: "route", Input: "hi", Voice: "alloy"}, map[string]json.RawMessage{"speed": json.RawMessage(`20`)}, nil},
		{"native control collision", &Request{Op: OpImageGeneration, Route: "route", Prompt: "p"}, map[string]json.RawMessage{"quality": json.RawMessage(`"high"`)}, map[string]json.RawMessage{"quality": json.RawMessage(`"low"`)}},
		{"multipart member collision", &Request{Op: OpTranscription, Route: "route", Extra: map[string]any{"vendor[0]": "caller"}}, nil, map[string]json.RawMessage{"vendor": json.RawMessage(`["default"]`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := mediaDefaultsConfig(t, test.request.Op, test.values, test.native)
			call, _, failure := EncodeConfigured(test.request, cfg, "native")
			if failure == nil || call != nil {
				t.Fatalf("unresolved default admitted: %+v", call)
			}
		})
	}
}

func TestConfiguredCloudImageDefaultsKeepQualifiedWrapperMappings(t *testing.T) {
	for _, test := range []struct{ id, kind, auth, model, native string }{
		{"vertex-gemini", "vertex_ai", "adc", "imagen-3.0-generate-002", "vertex_ai"},
		{"bedrock-converse", "bedrock", "default_chain", "amazon.titan-image-generator-v2:0", "bedrock"},
	} {
		t.Run(test.id, func(t *testing.T) {
			profile, err := connectors.LookupProfile(test.id, "1")
			if err != nil {
				t.Fatal(err)
			}
			cfg := connectors.Config{Kind: test.kind, AuthMode: test.auth, ProfileID: test.id, ProfileRevision: "1", OperationDefaults: map[string]connectors.DefaultSet{OpImageGeneration: {Dialect: profile.OperationDialect(OpImageGeneration), Values: map[string]json.RawMessage{"n": json.RawMessage(`2`), "size": json.RawMessage(`"1024x1024"`)}}}}
			r, failure := DecodeImageGeneration([]byte(`{"model":"images","prompt":"unaltered"}`))
			if failure != nil {
				t.Fatal(failure)
			}
			call, effective, failure := EncodeConfigured(r, cfg, test.model)
			if failure != nil {
				t.Fatal(failure)
			}
			if effective.Count == nil || *effective.Count != 2 || call.Native != test.native || bytes.Contains(call.JSON, []byte(`"n"`)) {
				t.Fatalf("wrapper changed: %s", call.JSON)
			}
			set := cfg.OperationDefaults[OpImageGeneration]
			set.NativeOptions = map[string]json.RawMessage{"parameters": json.RawMessage(`{"sampleCount":3}`)}
			cfg.OperationDefaults[OpImageGeneration] = set
			if call, _, failure := EncodeConfigured(r, cfg, test.model); failure == nil || call != nil || !strings.Contains(failure.Message, "parameters") {
				t.Fatal("unqualified wrapper extension accepted")
			}
			set.NativeOptions = nil
			cfg.OperationDefaults[OpImageGeneration] = set
			r, failure = DecodeImageGeneration([]byte(`{"model":"images","prompt":"unaltered","n":null}`))
			if failure != nil {
				t.Fatal(failure)
			}
			if call, _, failure := EncodeConfigured(r, cfg, test.model); failure == nil || call != nil {
				t.Fatal("cloud wrapper erased explicit native null")
			}
		})
	}
}
