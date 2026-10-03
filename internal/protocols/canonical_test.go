package protocols

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestGeminiCountKeepsNestedGenerationAndExtensions(t *testing.T) {
	fields, err := object([]byte(`{
		"model":"models/route",
		"generateContentRequest":{
			"model":"models/upstream",
			"contents":[{"role":"user","parts":[{"text":"hello"}]}],
			"generationConfig":{"maxOutputTokens":23,"vendor_option":true},
			"inner_extension":true
		},
		"outer_extension":true
	}`))
	if err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := decodeCanonical(openai.FamilyGeminiCount, fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(generation.Messages) != 1 || generation.Messages[0].Role != "user" || len(generation.Messages[0].Parts) != 1 || generation.Messages[0].Parts[0].Text != "hello" {
		t.Fatalf("nested messages were lost: %+v", generation.Messages)
	}
	if string(generation.Parameters["max_output_tokens"]) != "23" {
		t.Fatalf("nested parameters were lost: %v", generation.Parameters)
	}
	slices.Sort(generation.Extensions)
	want := []string{"/generationConfig/vendor_option", "/inner_extension", "/outer_extension"}
	if !slices.Equal(generation.Extensions, want) {
		t.Fatalf("extension paths = %v, want %v", generation.Extensions, want)
	}
	after, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("canonical decoding mutated the source envelope")
	}
}

// The OpenAI Agents SDK spells out defaults on every Responses request: an
// empty include list, non-strict tools, and the completed status of a tool
// result. None asks a target to preserve anything, so none may turn a
// translation into a refusal; a value that does ask for something still does.
func TestSpelledOutDefaultsAreNotSourceExtensions(t *testing.T) {
	responses := func(include, strict string) string {
		return `{"model":"route","include":` + include + `,"input":[
			{"role":"user","content":"weather?"},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{}","status":"completed"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny","status":"completed"}],
			"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"},"strict":` + strict + `}]}`
	}
	chat := func(strict string) string {
		return `{"model":"route","messages":[{"role":"user","content":"weather?"}],
			"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"},"strict":` + strict + `}}]}`
	}
	for _, test := range []struct {
		name   string
		family openai.Family
		body   string
		want   []string
	}{
		{"responses defaults", openai.FamilyResponses, responses(`[]`, `false`), nil},
		{"chat default strict", openai.FamilyChat, chat(`false`), nil},
		{"responses strict tool", openai.FamilyResponses, responses(`[]`, `true`), []string{"/tools/0/strict"}},
		{"chat strict tool", openai.FamilyChat, chat(`true`), []string{"/tools/0/function/strict"}},
		{"responses include request", openai.FamilyResponses, responses(`["reasoning.encrypted_content"]`, `false`), []string{"/include"}},
		{"responses defaults spelled with spaces", openai.FamilyResponses, responses(`[ ]`, ` false `), nil},
		{"responses incomplete tool output", openai.FamilyResponses, strings.Replace(responses(`[]`, `false`), `"output":"sunny","status":"completed"`, `"output":"sunny","status":"incomplete"`, 1), []string{"/input/2/status"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields, err := object([]byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			generation, err := decodeCanonical(test.family, fields)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(generation.Extensions, test.want) {
				t.Fatalf("extensions = %v, want %v", generation.Extensions, test.want)
			}
			if len(generation.Tools) != 1 || generation.Tools[0].Name != "get_weather" {
				t.Fatalf("the tool was lost: %+v", generation.Tools)
			}
			if test.family == openai.FamilyResponses && len(generation.Messages) != 3 {
				t.Fatalf("the tool round trip was lost: %+v", generation.Messages)
			}
		})
	}
}

func TestAgentsSDKResponsesRequestTranslatesToAnthropicAndGemini(t *testing.T) {
	body := `{"model":"route","include":[],"stream":false,"instructions":"Use the tool.","input":[
		{"role":"user","content":"weather?"},
		{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Oslo\"}","status":"completed"},
		{"type":"function_call_output","call_id":"call_1","output":"sunny","status":"completed"}],
		"tools":[{"type":"function","name":"get_weather","description":"Weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}},"strict":false}]}`
	for _, kind := range []string{"anthropic", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			r, err := Parse(openai.FamilyResponses, []byte(body), "")
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

// Current Google SDKs send tool and response schemas as standard JSON Schema in
// parametersJsonSchema and responseJsonSchema, and the Anthropic SDKs mark
// tools with eager_input_streaming when they stream. All three carry the same
// meaning a translated provider can represent, so none is a source extension.
func TestJSONSchemaFieldsAndStreamingHintsTranslate(t *testing.T) {
	const schema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`
	gemini := `{"contents":[{"role":"user","parts":[{"text":"weather?"}]}],
		"generationConfig":{"maxOutputTokens":32,"responseMimeType":"application/json","responseJsonSchema":` + schema + `},
		"tools":[{"functionDeclarations":[{"name":"get_weather","description":"Weather","parametersJsonSchema":` + schema + `}]}]}`
	anthropic := `{"model":"route","max_tokens":32,"messages":[{"role":"user","content":"weather?"}],
		"tools":[{"name":"get_weather","description":"Weather","input_schema":` + schema + `,"eager_input_streaming":true}]}`

	t.Run("gemini decodes both schemas", func(t *testing.T) {
		fields, err := object([]byte(gemini))
		if err != nil {
			t.Fatal(err)
		}
		generation, err := decodeCanonical(openai.FamilyGemini, fields)
		if err != nil {
			t.Fatal(err)
		}
		if len(generation.Extensions) != 0 {
			t.Fatalf("extensions = %v", generation.Extensions)
		}
		if len(generation.Tools) != 1 || string(generation.Tools[0].Schema) != schema {
			t.Fatalf("the tool schema was lost: %+v", generation.Tools)
		}
		var format struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		}
		if err := json.Unmarshal(generation.Parameters["response_format"], &format); err != nil || format.Type != "json_schema" || string(format.JSONSchema.Schema) != schema {
			t.Fatalf("the response schema was lost: %s (%v)", generation.Parameters["response_format"], err)
		}
	})
	t.Run("gemini refuses a schema given both ways", func(t *testing.T) {
		for _, body := range []string{
			`{"contents":[{"role":"user","parts":[{"text":"x"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT"},"parametersJsonSchema":{"type":"object"}}]}]}`,
			`{"contents":[{"role":"user","parts":[{"text":"x"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"OBJECT"},"responseJsonSchema":{"type":"object"}}}`,
		} {
			fields, err := object([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeCanonical(openai.FamilyGemini, fields); err == nil {
				t.Fatalf("accepted a schema given both ways: %s", body)
			}
		}
	})
	t.Run("anthropic ignores the streaming hint", func(t *testing.T) {
		fields, err := object([]byte(anthropic))
		if err != nil {
			t.Fatal(err)
		}
		generation, err := decodeCanonical(openai.FamilyAnthropic, fields)
		if err != nil {
			t.Fatal(err)
		}
		if len(generation.Extensions) != 0 || len(generation.Tools) != 1 || string(generation.Tools[0].Schema) != schema {
			t.Fatalf("extensions = %v, tools = %+v", generation.Extensions, generation.Tools)
		}
	})
	t.Run("both reach an OpenAI provider with the schema intact", func(t *testing.T) {
		for family, body := range map[openai.Family]string{openai.FamilyGemini: gemini, openai.FamilyAnthropic: anthropic} {
			r, err := Parse(family, []byte(body), "route")
			if err != nil {
				t.Fatal(err)
			}
			encoded, _, err := Encode(r, "openai", "openai", "wire-model", nil)
			if err != nil {
				t.Fatalf("%s: %v", family, err)
			}
			var out struct {
				Tools []struct {
					Function struct {
						Parameters json.RawMessage `json:"parameters"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(encoded, &out); err != nil || len(out.Tools) != 1 || string(out.Tools[0].Function.Parameters) != schema {
				t.Fatalf("%s: the tool schema was lost: %s", family, encoded)
			}
		}
	})
}

// LangChain sends an empty safetySettings list on every Gemini request and
// names the tool on every tool message. Neither asks a target to preserve
// anything; a real safety setting or the name of a participant still does.
func TestEmptySafetySettingsAndToolMessageNamesTranslate(t *testing.T) {
	gemini := func(settings string) string {
		return `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":` + settings + `}`
	}
	for _, test := range []struct {
		name, settings string
		want           []string
	}{
		{"empty", `[]`, nil},
		{"configured", `[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}]`, []string{"/safetySettings"}},
	} {
		t.Run("gemini safety settings "+test.name, func(t *testing.T) {
			fields, err := object([]byte(gemini(test.settings)))
			if err != nil {
				t.Fatal(err)
			}
			generation, err := decodeCanonical(openai.FamilyGemini, fields)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(generation.Extensions, test.want) {
				t.Fatalf("extensions = %v, want %v", generation.Extensions, test.want)
			}
		})
	}

	chat := func(role string) string {
		return `{"model":"route","max_tokens":32,"messages":[{"role":"user","content":"weather?"},
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
			{"role":"` + role + `","name":"get_weather","tool_call_id":"call_1","content":"sunny"}]}`
	}
	for _, kind := range []string{"anthropic", "gemini"} {
		t.Run("a named tool message reaches "+kind, func(t *testing.T) {
			r, err := Parse(openai.FamilyChat, []byte(chat("tool")), "")
			if err != nil {
				t.Fatal(err)
			}
			encoded, _, err := Encode(r, kind, kind, "wire-model", nil)
			if err != nil {
				t.Fatalf("a tool message with a name was refused: %v", err)
			}
			if !strings.Contains(string(encoded), "sunny") {
				t.Fatalf("the tool result was lost: %s", encoded)
			}
		})
	}
	t.Run("a named user message is still refused", func(t *testing.T) {
		body := `{"model":"route","max_tokens":32,"messages":[{"role":"user","name":"alice","content":"hi"}]}`
		r, err := Parse(openai.FamilyChat, []byte(body), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Encode(r, "anthropic", "anthropic", "wire-model", nil); err == nil {
			t.Fatal("dropped the name of a participant")
		}
	})
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
		generation, err := decodeCanonical(openai.FamilyGemini, fields)
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

// A Gemini client's structured output reaches another vendor with its
// properties in the order the client declared them.
func TestGeminiStructuredOutputKeepsThePropertyOrderOfTheClient(t *testing.T) {
	const request = `{"contents":[{"role":"user","parts":[{"text":"x"}]}],"generationConfig":{"responseMimeType":"application/json",` +
		`"responseSchema":{"type":"OBJECT","properties":{"title":{"type":"STRING"},"priority":{"type":"STRING"},"score":{"type":"INTEGER"}},"required":["title","priority","score"]}}}`
	r, err := Parse(openai.FamilyGemini, []byte(request), "route")
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := Encode(r, "openai", "openai", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	title, priority, score := bytes.Index(data, []byte(`"title"`)), bytes.Index(data, []byte(`"priority"`)), bytes.Index(data, []byte(`"score"`))
	if title < 0 || priority < title || score < priority {
		t.Fatalf("the properties were reordered: %s", data)
	}
}
