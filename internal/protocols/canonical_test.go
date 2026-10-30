package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

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

// A name written through an escape is lowered like any other, and the
// escapes that are left in the schema are kept as they were written.

// Each of Gemini's type names is lowered when it is the only one the schema
// has to give it away.

func TestMessagesToolsAndReasoningEncodeAsResponses(t *testing.T) {
	const body = `{"model":"route","max_tokens":64,"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"read_file","disable_parallel_tool_use":true},"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-read","name":"read_file","input":{"path":"main.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-read","content":"package main"}]}]}`
	request, err := Parse(wire.Anthropic, []byte(body), "")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := EncodeTarget(request, wire.Responses, "openai", "openai", "native-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	var target struct {
		Input []struct {
			Type                    string
			CallID                  string `json:"call_id"`
			Name, Arguments, Output string
		}
		Tools []struct {
			Type, Name, Description string
			Parameters              struct {
				Properties map[string]struct{ Type string }
			}
		}
		ToolChoice struct{ Type, Name string } `json:"tool_choice"`
		Parallel   *bool                       `json:"parallel_tool_calls"`
		Reasoning  struct{ Effort string }
		MaxTokens  int `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(encoded, &target); err != nil {
		t.Fatal(err)
	}
	if len(target.Input) != 2 || target.Input[0].Type != "function_call" || target.Input[0].CallID != "call-read" || target.Input[0].Name != "read_file" || target.Input[0].Arguments != `{"path":"main.go"}` || target.Input[1].Type != "function_call_output" || target.Input[1].CallID != target.Input[0].CallID || target.Input[1].Output != "package main" {
		t.Fatalf("tool history %s", encoded)
	}
	if len(target.Tools) != 1 || target.Tools[0].Type != "function" || target.Tools[0].Name != target.Input[0].Name || target.Tools[0].Description != "Read a file" || target.Tools[0].Parameters.Properties["path"].Type != "string" || target.ToolChoice.Type != "function" || target.ToolChoice.Name != target.Tools[0].Name || target.Parallel == nil || *target.Parallel || target.Reasoning.Effort != "high" || target.MaxTokens != 64 {
		t.Fatalf("tool and reasoning controls %s", encoded)
	}
}

func TestParseForModelPreservesExactNativeIdentity(t *testing.T) {
	model := "Native/" + strings.Repeat("Long", 50) + ":Version"
	body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"max_tokens":64}`)
	request, err := ParseForModel(wire.Anthropic, body, model)
	if err != nil || request.Route != model {
		t.Fatalf("native model %v %v", request, err)
	}
	encoded, _, err := EncodeTarget(request, wire.Responses, "openai", "openai", request.Route, nil)
	if err != nil {
		t.Fatal(err)
	}
	var forwarded struct{ Model string }
	if err := json.Unmarshal(encoded, &forwarded); err != nil || forwarded.Model != model {
		t.Fatalf("forwarded native model %s %v", encoded, err)
	}
	if _, err := ParseForModel(wire.Anthropic, body, strings.ToLower(model)); err == nil {
		t.Fatal("case-insensitive native model accepted")
	}
	if _, err := Parse(wire.Anthropic, body, ""); err == nil {
		t.Fatal("inference slug validation bypassed")
	}
}

func TestMessagesReasoningControlsArePreservedOrRefusedAcrossTargets(t *testing.T) {
	request, err := Parse(wire.Anthropic, []byte(`{"model":"team-model","messages":[{"role":"user","content":"Think carefully"}],"max_tokens":64,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		family           wire.Format
		provider, vendor string
		supported        bool
	}{
		{wire.Chat, "openai", "openai", true},
		{wire.Responses, "openai", "openai", true},
		{wire.Anthropic, "anthropic", "anthropic", true},
		{wire.Gemini, "gemini", "gemini", false},
		{"bedrock", "bedrock", "amazon-bedrock", false},
	} {
		t.Run(string(test.family), func(t *testing.T) {
			encoded, _, err := EncodeTarget(request, test.family, test.provider, test.vendor, "wire-model", nil)
			if !test.supported {
				if err == nil {
					t.Fatalf("reasoning control silently dropped %s", encoded)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var target struct {
				ReasoningEffort string `json:"reasoning_effort"`
				Reasoning       struct{ Effort string }
				OutputConfig    struct{ Effort string } `json:"output_config"`
			}
			if err := json.Unmarshal(encoded, &target); err != nil {
				t.Fatal(err)
			}
			effort := func() string {
				switch test.family {
				case wire.Chat:
					return target.ReasoningEffort
				case wire.Responses:
					return target.Reasoning.Effort
				default:
					return target.OutputConfig.Effort
				}
			}()
			if effort != "high" {
				t.Fatalf("reasoning control lost %s", encoded)
			}
		})
	}
}
