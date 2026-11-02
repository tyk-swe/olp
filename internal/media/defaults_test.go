package media

import (
	"encoding/json"
	"reflect"
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

func TestConfiguredMediaCanOverrideExistingDefaults(t *testing.T) {
	cfg := mediaDefaultsConfig(t, OpTranscription, map[string]json.RawMessage{
		"response_format":         json.RawMessage(`"unsupported"`),
		"timestamp_granularities": json.RawMessage(`[]`),
	}, nil)
	format := "verbose_json"
	request := &Request{
		Op: OpTranscription, Route: "route", File: &Part{}, Format: &format,
		TimestampGranularities: []string{"word"},
	}
	baselineCall, _, baselineFailure := EncodeConfigured(request, mediaDefaultsConfig(t, OpTranscription, nil, nil), "native")
	call, _, failure := EncodeConfigured(request, cfg, "native")
	if (failure == nil) != (baselineFailure == nil) {
		t.Fatalf("request with defaults failed: %v; without defaults failed: %v", failure, baselineFailure)
	}
	if baselineFailure == nil && !reflect.DeepEqual(call.Fields, baselineCall.Fields) {
		t.Fatalf("request with defaults sent fields %v; without defaults sent %v", call.Fields, baselineCall.Fields)
	}
}

func TestTranslationFormatsAgreeBetweenRequestsAndDefaults(t *testing.T) {
	for _, format := range []string{"json", "text", "srt", "vtt", "verbose_json", "unsupported"} {
		t.Run(format, func(t *testing.T) {
			raw, err := json.Marshal(format)
			if err != nil {
				t.Fatal(err)
			}
			cfg := mediaDefaultsConfig(t, OpTranslation, map[string]json.RawMessage{"response_format": raw}, nil)
			form := &Form{
				text:  map[string][]string{"model": {"route"}, "response_format": {format}},
				files: map[string][]Part{"file": {{}}},
			}
			_, failure := DecodeTranslation(form)
			if acceptedDefault, acceptedRequest := cfg.ValidateProfile() == nil, failure == nil; acceptedDefault != acceptedRequest {
				t.Fatalf("format %q accepted by default = %t, request = %t", format, acceptedDefault, acceptedRequest)
			}
		})
	}
}

func TestTranscriptionFormatsAgreeBetweenRequestsAndDefaults(t *testing.T) {
	for _, format := range []string{"json", "text", "srt", "vtt", "verbose_json", "diarized_json", "unsupported"} {
		t.Run(format, func(t *testing.T) {
			raw, err := json.Marshal(format)
			if err != nil {
				t.Fatal(err)
			}
			cfg := mediaDefaultsConfig(t, OpTranscription, map[string]json.RawMessage{"response_format": raw}, nil)
			form := &Form{
				text:  map[string][]string{"model": {"route"}, "response_format": {format}},
				files: map[string][]Part{"file": {{}}},
			}
			_, failure := DecodeTranscription(form)
			_, _, configuredFailure := EncodeConfigured(&Request{Op: OpTranscription, Route: "route", File: &Part{}}, cfg, "model")
			if acceptedDefault, acceptedRequest := configuredFailure == nil, failure == nil; acceptedDefault != acceptedRequest {
				t.Fatalf("format %q accepted by default = %t, request = %t", format, acceptedDefault, acceptedRequest)
			}
		})
	}
}
