package protocols

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestAgentsSDKResponsesRequestTranslatesToAnthropicAndGemini(t *testing.T) {
	body := `{"model":"route","include":[],"stream":false,"instructions":"Use the tool.","input":[
		{"role":"user","content":"weather?"},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Oslo\"}","status":"completed"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny","status":"completed"}],
		"tools":[{"type":"function","name":"get_weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}},"strict":false}]}`
	for _, kind := range []string{"anthropic", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			r, err := Parse(wire.Responses, []byte(body), "")
			if err != nil {
				t.Fatal(err)
			}
			encoded, _, err := Encode(r, kind, kind, "wire-model", Object{"max_tokens": raw(64)})
			if err != nil {
				t.Fatalf("a request the Agents SDK sends was refused: %v", err)
			}
			if !strings.Contains(string(encoded), "sunny") || !strings.Contains(string(encoded), "get_weather") {
				t.Fatalf("the tool round trip was lost: %s", encoded)
			}
		})
	}
}

// Gemini's own schema dialect spells types in capitals, and the Google SDKs
// emit it for a function's parameters. A provider that reads JSON Schema needs
// the standard names, and nothing else in the schema may change, the order of
// its members included: a structured output follows the order of the properties.
func TestGeminiSchemaTypesBecomeJSONSchemaTypes(t *testing.T) {
	const capitals = `{"type":"OBJECT","properties":{"title":{"type":"STRING"},"type":{"type":"STRING","enum":["OBJECT","ARRAY"]},` +
		`"tags":{"type":"ARRAY","items":{"type":"STRING"},"minItems":1},` +
		`"pair":{"type":"ARRAY","items":[{"type":"STRING"},{"type":"INTEGER"}]},` +
		`"count":{"type":"INTEGER","minimum":9007199254740993,"maximum":1.50},` +
		`"either":{"anyOf":[{"type":"STRING"},{"type":"NULL"}]},` +
		`"ref":{"oneOf":[{"type":"BOOLEAN"},{"type":"NUMBER"}],"not":{"type":"NULL"}},` +
		`"open":{"type":"OBJECT","additionalProperties":{"type":"STRING"},"patternProperties":{"^x":{"type":"INTEGER"}}},` +
		`"note":{"type":"STRING","description":"a <b> & c \u00e9 \"q\"","default":null,"nullable":true}},` +
		`"$defs":{"leaf":{"type":"STRING"}},"definitions":{"leaf":{"type":"ARRAY","prefixItems":[{"type":"INTEGER"}]}},` +
		`"required":["title","tags"],"additionalProperties":false}`
	const standard = `{"type":"object","properties":{"title":{"type":"string"},"type":{"type":"string","enum":["OBJECT","ARRAY"]},` +
		`"tags":{"type":"array","items":{"type":"string"},"minItems":1},` +
		`"pair":{"type":"array","items":[{"type":"string"},{"type":"integer"}]},` +
		`"count":{"type":"integer","minimum":9007199254740993,"maximum":1.50},` +
		`"either":{"anyOf":[{"type":"string"},{"type":"null"}]},` +
		`"ref":{"oneOf":[{"type":"boolean"},{"type":"number"}],"not":{"type":"null"}},` +
		`"open":{"type":"object","additionalProperties":{"type":"string"},"patternProperties":{"^x":{"type":"integer"}}},` +
		`"note":{"type":"string","description":"a <b> & c \u00e9 \"q\"","default":null,"nullable":true}},` +
		`"$defs":{"leaf":{"type":"string"}},"definitions":{"leaf":{"type":"array","prefixItems":[{"type":"integer"}]}},` +
		`"required":["title","tags"],"additionalProperties":false}`
	body := func(declaration, config string) string {
		return `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"generationConfig":{` + config + `},
			"tools":[{"functionDeclarations":[{"name":"f",` + declaration + `}]}]}`
	}
	canonical := func(t *testing.T, raw string) *Generation {
		t.Helper()
		fields, err := object([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		generation, err := decodeCanonical(wire.Gemini, fields)
		if err != nil {
			t.Fatal(err)
		}
		return generation
	}
	// The schema arrives as it was written, except for the type names.
	equal := func(t *testing.T, got json.RawMessage, want string) {
		t.Helper()
		if string(got) != want {
			t.Fatalf("schema = %s\nwant     %s", got, want)
		}
	}

	t.Run("function parameters", func(t *testing.T) {
		generation := canonical(t, body(`"parameters":`+capitals, ``))
		equal(t, generation.Tools[0].Schema, standard)
	})
	t.Run("response schema", func(t *testing.T) {
		generation := canonical(t, body(`"parametersJsonSchema":{"type":"object"}`, `"responseMimeType":"application/json","responseSchema":`+capitals))
		var format struct {
			JSONSchema struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		}
		if err := json.Unmarshal(generation.Parameters["response_format"], &format); err != nil {
			t.Fatal(err)
		}
		// Writing the response format escapes the characters HTML cares about,
		// which a JSON reader reads back as the same text.
		equal(t, format.JSONSchema.Schema, strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`).Replace(standard))
	})
	t.Run("a schema that is already standard keeps its bytes", func(t *testing.T) {
		generation := canonical(t, body(`"parameters":`+standard, ``))
		equal(t, generation.Tools[0].Schema, standard)
	})
	t.Run("names are written as they were", func(t *testing.T) {
		generation := canonical(t, body(`"parameters":{"type":"OBJECT","properties":{"caf\u00e9":{"type":"STRING"},"a\/b":{"type":"STRING","description":"\u003c"}}}`, ``))
		equal(t, generation.Tools[0].Schema, `{"type":"object","properties":{"caf\u00e9":{"type":"string"},"a\/b":{"type":"string","description":"\u003c"}}}`)
	})
	t.Run("whitespace and a member written twice survive a rewrite", func(t *testing.T) {
		generation := canonical(t, body(`"parameters":{ "type" : "OBJECT", "properties" : {"b":{"type":"STRING"},"a":{"type":"NUMBER"}},"properties":{"z":{"type":"BOOLEAN"}} }`, ``))
		equal(t, generation.Tools[0].Schema, `{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"number"}},"properties":{"z":{"type":"boolean"}}}`)
	})
	t.Run("parametersJsonSchema is never rewritten", func(t *testing.T) {
		const schema = `{"type":"object","properties":{"kind":{"type":"string","default":"OBJECT"}}}`
		generation := canonical(t, body(`"parametersJsonSchema":`+schema, ``))
		equal(t, generation.Tools[0].Schema, schema)
	})
	t.Run("a schema that cannot be read is passed on as it is", func(t *testing.T) {
		for _, schema := range []string{`{"type":"OBJECT"`, `{"type":"OBJECT",}`, `[`, `"OBJECT"`, `7`} {
			if got := jsonSchemaTypes(json.RawMessage(schema)); string(got) != schema {
				t.Fatalf("jsonSchemaTypes(%s) = %s", schema, got)
			}
		}
	})
}

// Gemini's parameters and responseSchema fields take an OpenAPI subset and
// refuse a member outside it, as OpenAI and Anthropic clients routinely send:
// $schema, additionalProperties, $ref. Their schemas reach Gemini in the fields
// that take JSON Schema as it is, which Google's own SDKs send them in, and a
// schema in the subset goes where it always did.
func TestGeminiSchemasGoInTheFieldThatTakesThem(t *testing.T) {
	const (
		subset = `{"type":"object","properties":{"city":{"type":"string","description":"a city"},"days":{"type":"integer","minimum":1},` +
			`"unit":{"type":"string","enum":["c","f"],"nullable":true},"tags":{"type":"array","items":{"type":"string"},"minItems":1},` +
			`"either":{"anyOf":[{"type":"string"},{"type":"integer"}]}},"required":["city"],"propertyOrdering":["city","days"]}`
		draft07 = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`
	)
	for _, tc := range []struct {
		name, schema string
		inSubset     bool
	}{
		{"a schema in the subset", subset, true},
		{"an empty schema", `{}`, true},
		{"a property named like a keyword", `{"type":"object","properties":{"additionalProperties":{"type":"string"},"$ref":{"type":"string"}}}`, true},
		{"$schema", draft07, false},
		{"additionalProperties at the root", `{"type":"object","properties":{},"additionalProperties":false}`, false},
		{"additionalProperties in a property", `{"type":"object","properties":{"a":{"type":"object","additionalProperties":{"type":"string"}}}}`, false},
		{"additionalProperties in an item", `{"type":"array","items":{"type":"object","additionalProperties":false}}`, false},
		{"additionalProperties in a branch", `{"anyOf":[{"type":"string"},{"type":"object","additionalProperties":false}]}`, false},
		{"$ref and $defs", `{"type":"object","properties":{"a":{"$ref":"#/$defs/a"}},"$defs":{"a":{"type":"string"}}}`, false},
		{"const", `{"type":"string","const":"x"}`, false},
		{"oneOf", `{"oneOf":[{"type":"string"},{"type":"integer"}]}`, false},
		{"a type that is a list", `{"type":["string","null"]}`, false},
		{"an enum of strings", `{"type":"string","enum":["a","b"]}`, true},
		{"an enum of numbers", `{"type":"integer","enum":[1,2,3]}`, false},
		{"an enum of numbers in a property", `{"type":"object","properties":{"n":{"type":"integer","enum":[1,2,3]}}}`, false},
		{"an enum of numbers in an item", `{"type":"array","items":{"type":"number","enum":[0.5,1.5]}}`, false},
		{"an enum of strings and a number", `{"enum":["a",2]}`, false},
		{"an enum of strings and null", `{"type":"string","enum":["a",null]}`, false},
		{"an enum of booleans", `{"type":"boolean","enum":[true]}`, false},
		{"an enum that is not a list", `{"type":"string","enum":"a"}`, false},
		{"a schema that is not an object", `true`, false},
		{"a list of items", `{"type":"array","items":[{"type":"string"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := inGeminiSchema(json.RawMessage(tc.schema)); got != tc.inSubset {
				t.Fatalf("inGeminiSchema(%s) = %v, want %v", tc.schema, got, tc.inSubset)
			}
			wantTool, wantFormat := "parameters", "responseSchema"
			if !tc.inSubset {
				wantTool, wantFormat = "parametersJsonSchema", "responseJsonSchema"
			}
			if tc.schema == "true" {
				return // not a schema an OpenAI request can carry
			}
			chat := `{"model":"route","max_tokens":32,"messages":[{"role":"user","content":"x"}],` +
				`"response_format":{"type":"json_schema","json_schema":{"name":"out","schema":` + tc.schema + `}},` +
				`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":` + tc.schema + `}}]}`
			r, err := Parse(wire.Chat, []byte(chat), "route")
			if err != nil {
				t.Fatal(err)
			}
			encoded, _, err := Encode(r, "gemini", "gemini", "wire-model", nil)
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				GenerationConfig map[string]json.RawMessage `json:"generationConfig"`
				Tools            []struct {
					FunctionDeclarations []map[string]json.RawMessage `json:"functionDeclarations"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(encoded, &out); err != nil || len(out.Tools) != 1 || len(out.Tools[0].FunctionDeclarations) != 1 {
				t.Fatalf("the tool was lost: %s (%v)", encoded, err)
			}
			declaration := out.Tools[0].FunctionDeclarations[0]
			// The schema arrives whole, in one field and not the other.
			for field, in := range map[string]map[string]json.RawMessage{
				"parameters": declaration, "parametersJsonSchema": declaration,
				"responseSchema": out.GenerationConfig, "responseJsonSchema": out.GenerationConfig,
			} {
				wanted := field == wantTool || field == wantFormat
				if got, present := in[field]; present != wanted || present && !jsonEqual(t, got, tc.schema) {
					t.Fatalf("%s = %s (present %v, want %v) in %s", field, got, present, wanted, encoded)
				}
			}
		})
	}
}

func jsonEqual(t *testing.T, a json.RawMessage, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		t.Fatalf("not JSON: %s %s", a, b)
	}
	return reflect.DeepEqual(x, y)
}

// A name written through an escape is lowered like any other, and the
// escapes that are left in the schema are kept as they were written.

// Each of Gemini's type names is lowered when it is the only one the schema
// has to give it away.
