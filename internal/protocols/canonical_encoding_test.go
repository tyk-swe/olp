package protocols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func decodeBody(t *testing.T, family openai.Family, body string) *Generation {
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

func encodeBody(t *testing.T, g *Generation, family openai.Family, operation string) map[string]any {
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

func TestToolOnlyAssistantTurnsEncodeNullChatContent(t *testing.T) {
	for _, tc := range []struct {
		family openai.Family
		body   string
	}{
		{openai.FamilyAnthropic, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`},
		{openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"t1","content":"ok"}]}`},
	} {
		t.Run(string(tc.family), func(t *testing.T) {
			out := encodeBody(t, decodeBody(t, tc.family, tc.body), openai.FamilyChat, "generation")
			assistant, _ := at(out, "messages", 1).(map[string]any)
			if content, ok := assistant["content"]; !ok || content != nil {
				t.Fatalf("assistant content = %#v, want null", content)
			}
			if at(assistant, "tool_calls", 0, "id") != "t1" {
				t.Fatalf("assistant = %#v", assistant)
			}
		})
	}
}

func TestToolsWithoutParametersEncodeValidSchemas(t *testing.T) {
	g := decodeBody(t, openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":5},"tools":[{"functionDeclarations":[{"name":"now","description":"time"},{"name":"get","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}]}`)
	anthropic := encodeBody(t, g, openai.FamilyAnthropic, "generation")
	schema, _ := json.Marshal(at(anthropic, "tools", 0, "input_schema"))
	if string(schema) != `{"properties":{},"type":"object"}` {
		t.Fatalf("anthropic input_schema = %s", schema)
	}
	if at(anthropic, "tools", 1, "input_schema", "properties", "q", "type") != "string" {
		t.Fatalf("anthropic dropped declared schema: %#v", at(anthropic, "tools", 1))
	}
	chat := encodeBody(t, g, openai.FamilyChat, "generation")
	if _, ok := at(chat, "tools", 0, "function").(map[string]any)["parameters"]; ok {
		t.Fatalf("chat tool has parameters: %#v", at(chat, "tools", 0))
	}
	if at(chat, "tools", 1, "function", "parameters", "properties", "q", "type") != "string" {
		t.Fatalf("chat dropped declared schema: %#v", at(chat, "tools", 1))
	}
	count := encodeBody(t, g, openai.FamilyInputTokens, "token_count")
	if _, ok := at(count, "tools", 0).(map[string]any)["parameters"]; ok {
		t.Fatalf("responses tool has parameters: %#v", at(count, "tools", 0))
	}
	chatSource := decodeBody(t, openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"now"}}]}`)
	gemini := encodeBody(t, chatSource, openai.FamilyGemini, "generation")
	if _, ok := at(gemini, "tools", 0, "functionDeclarations", 0).(map[string]any)["parameters"]; ok {
		t.Fatalf("gemini declaration has parameters: %#v", at(gemini, "tools"))
	}
}

func TestEmptyTextPartsAreNotEncoded(t *testing.T) {
	r := parse(t, openai.FamilyChat, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":""}]}`)
	for _, kind := range []string{"anthropic", "gemini"} {
		body, _, err := Encode(r, kind, kind, "wire-model", nil)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if strings.Contains(string(body), `"text":""`) {
			t.Fatalf("%s body has an empty text part: %s", kind, body)
		}
		if kind != "anthropic" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		assistant, _ := at(v, "messages", 1, "content").([]any)
		if len(assistant) != 1 || at(assistant, 0, "type") != "tool_use" {
			t.Fatalf("assistant content = %#v", assistant)
		}
		result, _ := at(v, "messages", 2, "content", 0).(map[string]any)
		if _, ok := result["content"]; ok || result["type"] != "tool_result" {
			t.Fatalf("tool_result = %#v", result)
		}
	}
}

func TestGeminiGroupsParallelToolResultsIntoOneTurn(t *testing.T) {
	g := decodeBody(t, openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"x","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"y","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"1"},{"role":"tool","tool_call_id":"b","content":"2"},{"role":"user","content":"again"},{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"x","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"3"}]}`)
	out := encodeBody(t, g, openai.FamilyGemini, "generation")
	contents, _ := out["contents"].([]any)
	if len(contents) != 6 {
		t.Fatalf("contents = %#v", contents)
	}
	grouped, _ := at(contents, 2, "parts").([]any)
	if at(contents, 2, "role") != "user" || len(grouped) != 2 || at(grouped, 0, "functionResponse", "id") != "a" || at(grouped, 1, "functionResponse", "id") != "b" {
		t.Fatalf("tool turn = %#v", at(contents, 2))
	}
	if at(contents, 3, "parts", 0, "text") != "again" || at(contents, 5, "parts", 0, "functionResponse", "id") != "c" {
		t.Fatalf("later turns merged: %#v", contents)
	}
}

func TestTranslatedCountToOpenAIUsesResponsesShape(t *testing.T) {
	r := parse(t, openai.FamilyGeminiCount, `{"generateContentRequest":{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"yo"}]}],"generationConfig":{"maxOutputTokens":5,"temperature":0.2,"stopSequences":["x"],"responseMimeType":"application/json"}}}`)
	body, wire, err := Encode(r, "openai", "openai", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if wire != openai.FamilyInputTokens {
		t.Fatalf("wire = %s", wire)
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

	r = parse(t, openai.FamilyAnthropicCount, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"f"}}`)
	body, _, err = Encode(r, "openai", "openai", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"tool_choice":{"name":"f","type":"function"}`) {
		t.Fatalf("tool_choice not in Responses shape: %s", body)
	}
}

func TestCountRequestsOmitGenerationDefaults(t *testing.T) {
	defaults := Object{"max_tokens": json.RawMessage(`100`), "temperature": json.RawMessage(`0.2`)}
	for _, tc := range []struct {
		family openai.Family
		kind   string
		body   string
	}{
		{openai.FamilyAnthropicCount, "anthropic", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{openai.FamilyInputTokens, "openai", `{"model":"m","input":"hi"}`},
		{openai.FamilyInputTokens, "anthropic", `{"model":"m","input":"hi"}`},
		{openai.FamilyGeminiCount, "gemini", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	} {
		t.Run(string(tc.family)+"->"+tc.kind, func(t *testing.T) {
			body, _, err := Encode(parse(t, tc.family, tc.body), tc.kind, tc.kind, "wire-model", defaults)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"max_tokens", "temperature", "max_output_tokens"} {
				if strings.Contains(string(body), `"`+key+`"`) {
					t.Fatalf("count body has %s: %s", key, body)
				}
			}
		})
	}
	body, _, err := Encode(parse(t, openai.FamilyChat, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "anthropic", "anthropic", "wire-model", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"max_tokens":100`) {
		t.Fatalf("generation lost provider defaults: %s", body)
	}
}
