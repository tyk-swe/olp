package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func decodeBody(t *testing.T, family wire.Format, body string) *Generation {
	t.Helper()
	fields := Object{}
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	g, err := decodeCanonical(family, fields)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func encodeBody(t *testing.T, g *Generation, family wire.Format, operation string) map[string]any {
	t.Helper()
	out, err := encodeCanonical(g, family, "m", operation)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func at(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			a, _ := v.([]any)
			if k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func TestTranslatedCountToOpenAIUsesResponsesShape(t *testing.T) {
	r := parse(t, wire.GeminiCount, `{"generateContentRequest":{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"yo"}]}],"generationConfig":{"maxOutputTokens":5,"temperature":0.2,"stopSequences":["x"],"responseMimeType":"application/json"}}}`)
	body, encodedWire, err := Encode(r, "openai", "openai", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if encodedWire != wire.InputTokens {
		t.Fatalf("wire = %s", encodedWire)
	}
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"max_completion_tokens", "max_output_tokens", "temperature", "stop", "response_format"} {
		if _, ok := v[key]; ok {
			t.Fatalf("count body has %s: %s", key, body)
		}
	}
	if at(v, "text", "format", "type") != "json_object" || at(v, "input", 1, "content", 0, "type") != "output_text" {
		t.Fatalf("count body = %s", body)
	}

	r = parse(t, wire.AnthropicCount, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"f"}}`)
	body, _, err = Encode(r, "openai", "openai", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"tool_choice":{"name":"f","type":"function"}`) {
		t.Fatalf("tool_choice not in Responses shape: %s", body)
	}
}
