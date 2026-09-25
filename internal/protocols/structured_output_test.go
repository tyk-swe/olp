package protocols

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func requestWithFormat(t *testing.T, format string, stream bool) *openai.Request {
	t.Helper()
	fields, _ := object([]byte(`{"model":"team-model","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`))
	if format != "" {
		fields["response_format"] = json.RawMessage(format)
	}
	if stream {
		fields["stream"] = json.RawMessage("true")
	}
	r, err := Parse(openai.FamilyChat, raw(fields), "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func jsonField(t *testing.T, body []byte, key string) map[string]any {
	t.Helper()
	var f map[string]json.RawMessage
	if err := json.Unmarshal(body, &f); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(f[key], &v); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return v
}

func jsonValue(t *testing.T, literal string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(literal), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

const schemaFormat = `{"type":"json_schema","json_schema":{"name":"answer","description":"the answer","schema":{"type":"object","properties":{"x":{"type":"string"}}},"strict":true}}`

func TestAnthropicStructuredOutputTranslation(t *testing.T) {
	for _, format := range []string{"", `{"type":"text"}`} {
		body, _, err := Encode(requestWithFormat(t, format, false), "anthropic", "anthropic", "wire-model", nil)
		if err != nil {
			t.Fatalf("format %q: %v", format, err)
		}
		var f map[string]json.RawMessage
		if err := json.Unmarshal(body, &f); err != nil {
			t.Fatal(err)
		}
		if _, ok := f["output_config"]; ok {
			t.Fatalf("format %q emitted output_config: %s", format, body)
		}
	}
	body, _, err := Encode(requestWithFormat(t, schemaFormat, false), "anthropic", "anthropic", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := jsonValue(t, `{"format":{"type":"json_schema","schema":{"type":"object","properties":{"x":{"type":"string"}}}}}`)
	if got := jsonField(t, body, "output_config"); !reflect.DeepEqual(got, want) {
		t.Fatalf("output_config %v, want %v", got, want)
	}
	for _, format := range []string{
		`{"type":"json_schema","json_schema":{"name":"a","schema":{"type":"object"},"strict":false}}`,
		`{"type":"json_schema","json_schema":{"name":"a","schema":"not-an-object"}}`,
		`{"type":"json_schema","json_schema":{"name":"a"}}`,
		`{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}`,
		`{"type":"json_schema","json_schema":{"name":"","schema":{"type":"object"}}}`,
		`{"type":"json_schema","json_schema":{"name":"bad name!","schema":{"type":"object"}}}`,
		`{"type":"json_schema","json_schema":{"name":"a","description":7,"schema":{"type":"object"}}}`,
		`{"type":"json_schema","json_schema":"oops"}`,
		`{"type":"json_object"}`,
		`{"type":"bogus"}`,
	} {
		if _, _, err := Encode(requestWithFormat(t, format, false), "anthropic", "anthropic", "wire-model", nil); err == nil {
			t.Errorf("accepted %s", format)
		}
	}
	fields, _ := object([]byte(`{"model":"team-model","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`))
	fields["seed"] = json.RawMessage("7")
	r, err := Parse(openai.FamilyChat, raw(fields), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Encode(r, "anthropic", "anthropic", "wire-model", nil); err == nil {
		t.Error("seed must stay refused")
	}
}

func TestBedrockStructuredOutputTranslation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		body, _, err := Encode(requestWithFormat(t, schemaFormat, stream), "bedrock", "amazon-bedrock", "wire-model", nil)
		if err != nil {
			t.Fatalf("stream=%v: %v", stream, err)
		}
		got := jsonField(t, body, "outputConfig")
		textFormat, _ := got["textFormat"].(map[string]any)
		structure, _ := textFormat["structure"].(map[string]any)
		jsonSchema, _ := structure["jsonSchema"].(map[string]any)
		if jsonSchema == nil || textFormat["type"] != "json_schema" {
			t.Fatalf("stream=%v outputConfig %v", stream, got)
		}
		if jsonSchema["name"] != "answer" || jsonSchema["description"] != "the answer" {
			t.Fatalf("stream=%v jsonSchema %v", stream, jsonSchema)
		}
		want := `{"type":"object","properties":{"x":{"type":"string"}}}`
		if jsonSchema["schema"] != want {
			t.Fatalf("stream=%v schema %v, want %s", stream, jsonSchema["schema"], want)
		}
	}
	body, _, err := Encode(requestWithFormat(t, `{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}`, false), "bedrock", "amazon-bedrock", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	jsonSchema := jsonField(t, body, "outputConfig")["textFormat"].(map[string]any)["structure"].(map[string]any)["jsonSchema"].(map[string]any)
	if _, ok := jsonSchema["description"]; ok || jsonSchema["schema"] != `{"type":"object"}` {
		t.Fatalf("jsonSchema %v", jsonSchema)
	}
	for _, format := range []string{`{"type":"text"}`} {
		body, _, err := Encode(requestWithFormat(t, format, false), "bedrock", "amazon-bedrock", "wire-model", nil)
		if err != nil {
			t.Fatal(err)
		}
		var f map[string]json.RawMessage
		if err := json.Unmarshal(body, &f); err != nil {
			t.Fatal(err)
		}
		if _, ok := f["outputConfig"]; ok {
			t.Fatalf("format %q emitted outputConfig: %s", format, body)
		}
	}
	for _, format := range []string{
		`{"type":"json_schema","json_schema":{"schema":{"type":"object"}}}`,
		`{"type":"json_schema","json_schema":{"name":"","schema":{"type":"object"}}}`,
		`{"type":"json_schema","json_schema":{"name":"bad name!","schema":{"type":"object"}}}`,
		`{"type":"json_schema","json_schema":{"name":"a","description":{"x":1},"schema":{"type":"object"}}}`,
		`{"type":"json_schema","json_schema":{"name":"a","schema":{"type":"object"},"strict":false}}`,
		`{"type":"json_schema","json_schema":{"name":"a","schema":[1]}}`,
		`{"type":"json_object"}`,
	} {
		for _, stream := range []bool{false, true} {
			if _, _, err := Encode(requestWithFormat(t, format, stream), "bedrock", "amazon-bedrock", "wire-model", nil); err == nil {
				t.Errorf("stream=%v accepted %s", stream, format)
			}
		}
	}
}
