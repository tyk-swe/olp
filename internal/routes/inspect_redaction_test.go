package routes

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func inspected(t *testing.T, body string, provenance ...oif.Provenance) inspectedRequest {
	t.Helper()
	document, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return inspectRequest(document, provenance)
}

func TestInspectionNeverRepeatsCallerContent(t *testing.T) {
	const private = "zebra-7f3a"
	requests := map[string]string{
		"a chat completion": `{"model":"C","C":"C","user":"C","metadata":{"C":"C"},"stop":["C"],
			"reasoning_effort":"C","service_tier":"C","verbosity":"C","temperature":"C","stream":"C",
			"messages":[{"role":"C","content":"C"},
				{"role":"user","content":[{"type":"text","text":"C"},{"type":"C","C":"C"}]},
				{"role":"assistant","tool_calls":[{"id":"C","type":"function","function":{"name":"C","arguments":"{\"C\":\"C\"}"}}]},
				{"role":"tool","tool_call_id":"C","content":"C"}],
			"tools":[{"type":"function","function":{"name":"C","parameters":{"type":"object","properties":{"C":{"type":"string"}}}}}],
			"tool_choice":{"type":"function","function":{"name":"C"}},
			"response_format":{"type":"json_schema","json_schema":{"name":"C"}}}`,
		"a response": `{"instructions":"C","previous_response_id":"C","truncation":"C",
			"input":[{"type":"function_call","call_id":"C","name":"C","arguments":"C"},
				{"type":"function_call_output","call_id":"C","output":"C"},
				{"role":"user","content":"C"}],
			"reasoning":{"effort":"C","summary":"C","C":"C"}}`,
		"a message": `{"model":"C","system":[{"type":"text","text":"C"}],
			"messages":[{"role":"user","content":[{"type":"tool_use","id":"C","name":"C","input":{"C":"C"}},
				{"type":"tool_result","tool_use_id":"C","content":"C"}]}],
			"thinking":{"type":"C","budget_tokens":"C","C":"C"}}`,
		"generated content": `{"systemInstruction":{"parts":[{"text":"C"}]},
			"contents":[{"role":"C","parts":[{"text":"C"},{"functionCall":{"name":"C","args":{"C":"C"}}},{"C":"C"}]}],
			"generationConfig":{"temperature":"C","C":"C","stopSequences":["C"]},
			"safetySettings":[{"category":"C"}]}`,
	}
	for name, body := range requests {
		summary := inspected(t, strings.ReplaceAll(body, "C", private),
			oif.Provenance{Pointer: "/model", Origin: oif.Origin(private), Reason: private},
			oif.Provenance{Pointer: "/" + private, Origin: oif.Caller, Reason: private})
		shown, err := json.Marshal(summary)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(shown), private) {
			t.Errorf("inspecting %s repeats caller content: %s", name, shown)
		}
		if len(summary.Fields) == 0 || len(summary.Structure) == 0 || summary.RedactedNativeFields == 0 {
			t.Errorf("inspecting %s describes nothing: %s", name, shown)
		}
	}
}

func TestInspectionShowsOnlyKnownSafeSettings(t *testing.T) {
	summary := inspected(t, `{"model":"fixture-model","messages":[{"role":"user","content":"hi"}],"tools":[],
		"temperature":0.2,"seed":null,"stream":true,"tool_choice":"auto","reasoning_effort":"high","service_tier":"fastest",
		"top_p":`+strings.Repeat("1", 300)+`,"vendor_option":true,
		"thinking":{"type":"enabled","budget_tokens":1024,"display":"summarized"},
		"generationConfig":{"topK":40,"responseSchema":{"type":"object"}}}`)
	shown, empty := map[string]string{}, map[string]bool{}
	for _, field := range summary.Fields {
		if field.Redacted != (field.ValueJSON == nil) {
			t.Errorf("%s: redacted is %t with value %v", field.Field, field.Redacted, field.ValueJSON)
		}
		if field.ValueJSON != nil {
			shown[field.Field] = *field.ValueJSON
		}
		if field.Empty != nil {
			empty[field.Field] = *field.Empty
		}
	}
	want := map[string]string{
		"/temperature": "0.2", "/seed": "null", "/stream": "true", "/tool_choice": `"auto"`, "/reasoning_effort": `"high"`,
		"/thinking/type": `"enabled"`, "/thinking/budget_tokens": "1024", "/generationConfig/topK": "40",
	}
	if !maps.Equal(shown, want) {
		t.Errorf("shown values: got %v, want %v", shown, want)
	}
	if messages, known := empty["/messages"]; !known || messages || !empty["/tools"] {
		t.Errorf("emptiness: got %v, want filled messages and empty tools", empty)
	}
	if summary.RedactedNativeFields != 3 {
		t.Errorf("unlisted native fields: got %d, want 3", summary.RedactedNativeFields)
	}
}

func outline(turns []inspectedTurn) []string {
	lines := make([]string, len(turns))
	for i, turn := range turns {
		parts := make([]string, len(turn.Parts))
		for j, part := range turn.Parts {
			parts[j] = part.Kind
			if part.CallOrdinal != nil {
				parts[j] = fmt.Sprintf("%s#%d", part.Kind, *part.CallOrdinal)
			}
		}
		lines[i] = fmt.Sprintf("%s[%d] %s: %s", turn.Scope, turn.Index, turn.Role, strings.Join(parts, " "))
	}
	return lines
}

func TestInspectionLinksToolResultsToTheirCallsByOrdinal(t *testing.T) {
	conversations := map[string]struct {
		body string
		want []string
	}{
		"chat messages": {`{"messages":[{"role":"system","content":"s"},
			{"role":"user","content":[{"type":"text","text":"t"},{"type":"image_url","image_url":{"url":"u"}}]},
			{"role":"assistant","content":null,"tool_calls":[{"id":"first"},{"id":"second"}]},
			{"role":"tool","tool_call_id":"second","content":"r"},
			{"role":"tool","tool_call_id":"first","content":"r"},
			{"role":"tool","tool_call_id":"never called","content":"r"}]}`, []string{
			"messages[0] system: text", "messages[1] user: text image", "messages[2] assistant: null tool_call#1 tool_call#2",
			"messages[3] tool: tool_result#2", "messages[4] tool: tool_result#1", "messages[5] tool: tool_result",
		}},
		"response items": {`{"instructions":"i","input":[{"role":"user","content":[{"type":"input_text","text":"t"},{"type":"input_file","file_id":"f"}]},
			{"type":"function_call","call_id":"first"},{"type":"reasoning"},{"type":"function_call_output","call_id":"first"}]}`, []string{
			"instructions[0] instruction: text", "input[0] user: text document", "input[1] unspecified: tool_call#1",
			"input[2] unspecified: ", "input[3] unspecified: tool_result#1",
		}},
		"message blocks": {`{"system":"s","messages":[{"role":"assistant","content":[{"type":"thinking"},{"type":"tool_use","id":"first"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"first"},{"type":"text","text":"t"}]}]}`, []string{
			"system[0] system: text", "messages[0] assistant: reasoning tool_call#1", "messages[1] user: tool_result#1 text",
		}},
		"generated content": {`{"systemInstruction":{"parts":[{"text":"s"}]},"contents":[{"role":"user","parts":[{"text":"t"},{"inlineData":{}}]},
			{"role":"model","parts":[{"functionCall":{}}]},{"role":"function","parts":[{"functionResponse":{}}]}]}`, []string{
			"system_instruction[0] system: text", "contents[0] user: text media", "contents[1] model: tool_call", "contents[2] function: tool_result",
		}},
	}
	for name, conversation := range conversations {
		if got := outline(inspected(t, conversation.body).Structure); !slices.Equal(got, conversation.want) {
			t.Errorf("%s:\n got %q\nwant %q", name, got, conversation.want)
		}
	}
}

func TestInspectionBoundsTheStructureItReturns(t *testing.T) {
	block := `{"type":"text","text":"t"}`
	long := `{"role":"user","content":[` + strings.TrimSuffix(strings.Repeat(block+",", maxInspectedParts+4), ",") + `]}`
	short := `{"role":"user","content":"t"}`
	summary := inspected(t, `{"messages":[`+long+strings.Repeat(","+short, maxInspectedTurns+5)+`]}`)
	if len(summary.Structure) != maxInspectedTurns || summary.OmittedTurns != 6 {
		t.Errorf("turns: kept %d and omitted %d, want %d and 6", len(summary.Structure), summary.OmittedTurns, maxInspectedTurns)
	}
	if first := summary.Structure[0]; len(first.Parts) != maxInspectedParts || first.OmittedParts != 4 {
		t.Errorf("parts: kept %d and omitted %d, want %d and 4", len(first.Parts), first.OmittedParts, maxInspectedParts)
	}
}
