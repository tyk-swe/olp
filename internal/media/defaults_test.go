package media

import (
	"encoding/json"
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
