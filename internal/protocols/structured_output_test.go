package protocols

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func requestWithFormat(t *testing.T, format string, stream bool) *wire.Request {
	t.Helper()
	fields, _ := object([]byte(generationFixtures[wire.Chat]))
	if format != "" {
		fields["response_format"] = json.RawMessage(format)
	}
	if stream {
		fields["stream"] = json.RawMessage("true")
	}
	r, err := Parse(wire.Chat, raw(fields), "")
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
	fields, _ := object([]byte(generationFixtures[wire.Chat]))
	fields["seed"] = json.RawMessage("7")
	r, err := Parse(wire.Chat, raw(fields), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Encode(r, "anthropic", "anthropic", "wire-model", nil); err == nil {
		t.Error("seed must stay refused")
	}
}
