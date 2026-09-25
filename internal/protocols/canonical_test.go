package protocols

import (
	"encoding/json"
	"slices"
	"strconv"
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

// OpenAI Responses forces a function with {"type":"function","name"}; Chat uses
// {"type":"function","function":{"name"}}. Each destination gets its own form.
func TestResponsesNamedToolChoiceTranslatesToEachDestination(t *testing.T) {
	body := `{"model":"route","input":"weather?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"get_weather"}}`
	for _, tc := range []struct {
		kind, vendor, path, want string
	}{
		{"openai", "deepseek", "/tool_choice", `{"function":{"name":"get_weather"},"type":"function"}`},
		{"anthropic", "anthropic", "/tool_choice", `{"name":"get_weather","type":"tool"}`},
		{"gemini", "gemini", "/toolConfig/functionCallingConfig", `{"allowedFunctionNames":["get_weather"],"mode":"ANY"}`},
		{"bedrock", "bedrock", "/toolConfig/toolChoice", `{"tool":{"name":"get_weather"}}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			request, err := openai.Parse(openai.FamilyResponses, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			encoded, _, err := Encode(request, tc.kind, tc.vendor, "m", Object{"max_tokens": raw(64)})
			if err != nil {
				t.Fatalf("named Responses tool choice refused: %v", err)
			}
			if got := jsonAt(t, encoded, tc.path); got != tc.want {
				t.Fatalf("%s = %s, want %s", tc.path, got, tc.want)
			}
		})
	}
	count, err := Parse(openai.FamilyAnthropicCount, []byte(`{"model":"route","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"get_weather"}}`), "route")
	if err != nil {
		t.Fatal(err)
	}
	encoded, wire, err := Encode(count, "openai", "openai", "m", nil)
	if err != nil || wire != openai.FamilyInputTokens {
		t.Fatalf("wire=%s err=%v", wire, err)
	}
	if got := jsonAt(t, encoded, "/tool_choice"); got != `{"name":"get_weather","type":"function"}` {
		t.Fatalf("Responses input_tokens tool_choice = %s", got)
	}
}

// Chat assistant content is nullable and, as an array, needs at least one part.
func TestToolCallOnlyAssistantTurnUsesNullChatContent(t *testing.T) {
	anthropic, err := Parse(openai.FamilyAnthropic, []byte(`{"model":"route","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"get_weather","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"sunny"}]}],"tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`), "route")
	if err != nil {
		t.Fatal(err)
	}
	responses, err := openai.Parse(openai.FamilyResponses, []byte(`{"model":"route","input":[{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"sunny"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		request      *openai.Request
		vendor, path string
	}{{anthropic, "openai", "/messages/1/content"}, {responses, "deepseek", "/messages/0/content"}} {
		encoded, wire, err := Encode(tc.request, "openai", tc.vendor, "m", nil)
		if err != nil || wire != openai.FamilyChat {
			t.Fatalf("wire=%s err=%v", wire, err)
		}
		if got := jsonAt(t, encoded, tc.path); got != "null" {
			t.Fatalf("%s: tool-call turn content = %s in %s", tc.request.Family, got, encoded)
		}
	}
}

// Gemini expects all function responses for a parallel function-call turn in
// the following user content ("FC1, FC2, FR1, FR2").
func TestParallelToolResultsShareOneGeminiContent(t *testing.T) {
	chat, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"user","content":"weather?"},`+
		`{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"w","arguments":"{\"c\":\"Paris\"}"}},{"id":"b","type":"function","function":{"name":"w","arguments":"{\"c\":\"Rome\"}"}}]},`+
		`{"role":"tool","tool_call_id":"a","content":"20"},{"role":"tool","tool_call_id":"b","content":"25"},{"role":"user","content":"thanks"}],`+
		`"tools":[{"type":"function","function":{"name":"w","parameters":{"type":"object"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := Encode(chat, "gemini", "gemini", "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"parts":[{"text":"weather?"}],"role":"user"},` +
		`{"parts":[{"functionCall":{"args":{"c":"Paris"},"id":"a","name":"w"}},{"functionCall":{"args":{"c":"Rome"},"id":"b","name":"w"}}],"role":"model"},` +
		`{"parts":[{"functionResponse":{"id":"a","name":"w","response":{"output":"20"}}},{"functionResponse":{"id":"b","name":"w","response":{"output":"25"}}}],"role":"user"},` +
		`{"parts":[{"text":"thanks"}],"role":"user"}]`
	if got := jsonAt(t, encoded, "/contents"); got != want {
		t.Fatalf("contents = %s", got)
	}
}

func jsonAt(t *testing.T, document []byte, pointer string) string {
	t.Helper()
	var value any
	if err := json.Unmarshal(document, &value); err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch node := value.(type) {
		case map[string]any:
			value = node[name]
		case []any:
			index, err := strconv.Atoi(name)
			if err != nil || index >= len(node) {
				t.Fatalf("%s is not present in %s", pointer, document)
			}
			value = node[index]
		default:
			t.Fatalf("%s is not present in %s", pointer, document)
		}
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
