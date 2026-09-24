package connectors

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestAudioTranslationProfileAndDefaultContract(t *testing.T) {
	for _, id := range []string{"openai-chat", "openai-responses", "compatible-chat", "azure-v1-responses", "azure-legacy-chat"} {
		p, err := LookupProfile(id, "1")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(p.Operations, "translation") || !Supports(p.Kind, "", "translation", "openai", "unary") || Supports(p.Kind, "", "translation", "openai", "streaming") {
			t.Fatalf("translation profile modes: %+v", p)
		}
		var schema map[string]any
		if json.Unmarshal(p.DefaultSchemas["translation"], &schema) != nil {
			t.Fatal("missing translation defaults schema")
		}
		defaults := DefaultSet{Dialect: p.OperationDialect("translation"), Values: map[string]json.RawMessage{"prompt": json.RawMessage(`"English hint"`), "temperature": json.RawMessage(`0`), "response_format": json.RawMessage(`"vtt"`)}}
		if err := validateDefaultSet(p, "translation", defaults); err != nil {
			t.Fatal(err)
		}
		for name, raw := range map[string]string{"temperature": "1.1", "response_format": `"diarized_json"`, "language": `"fr"`, "prompt": `42`} {
			changed := DefaultSet{Dialect: defaults.Dialect, Values: map[string]json.RawMessage{name: json.RawMessage(raw)}}
			if err := validateDefaultSet(p, "translation", changed); err == nil {
				t.Fatalf("invalid translation default %s accepted", name)
			}
		}
	}
}
