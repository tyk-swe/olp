package routes

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestInspectionKeepsSettingsAndPresenceWithoutContent(t *testing.T) {
	document, err := oif.ParseJSON([]byte(`{
		"messages":[{"role":"user","content":"private-prompt-marker"}],
		"tools":[{"name":"private-tool-marker","input_schema":{"private-schema-key":true}}],
		"thinking":{"type":"enabled","budget_tokens":1024,"private-thinking-name":"private-signature-marker"},
		"temperature":0,"top_p":null,"parallel_tool_calls":false,"stop":[],"seed":9007199254740993,
		"service_tier":"private-tier-marker","private-native-key":{"value":"private-native-value"}
	}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	result := inspectRequest(document, []oif.Provenance{{Pointer: "/thinking", Origin: oif.ProviderDefault}})
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-") {
		t.Fatal("inspection exposed content, schema names, unknown enum values or native property names")
	}
	fields := map[string]inspectedField{}
	for _, field := range result.Fields {
		fields[field.Field] = field
	}
	for pointer, expected := range map[string]string{"/temperature": "0", "/top_p": "null", "/parallel_tool_calls": "false", "/thinking/type": `"enabled"`, "/thinking/budget_tokens": "1024", "/seed": "9007199254740993"} {
		field := fields[pointer]
		if field.Redacted || field.ValueJSON == nil || *field.ValueJSON != expected {
			t.Fatalf("safe setting or explicit presence was lost at %s", pointer)
		}
	}
	if fields["/thinking/budget_tokens"].Origin != "provider_default" || fields["/stop"].Kind != "array" || !fields["/stop"].Redacted {
		t.Fatal("default provenance or empty-container kind was lost")
	}
	if fields["/stop"].Empty == nil || !*fields["/stop"].Empty {
		t.Fatal("empty native array presence was lost")
	}
	if result.RedactedNativeFields != 2 {
		t.Fatal("unknown native fields were not counted without their names")
	}
}

func TestInspectionDiagnosticsCannotExposeArbitraryPointerNames(t *testing.T) {
	for pointer, want := range map[string]string{
		"/messages/0/content/private-prompt-marker":   "/messages",
		"/tools/0/input_schema/private-schema-marker": "/tools",
		"/private-native-key":                         "/native_fields",
		"/thinking/budget_tokens":                     "/thinking/budget_tokens",
	} {
		if inspectionField(pointer) != want {
			t.Fatal("inspection diagnostic exposed a private pointer or lost a safe control")
		}
	}
}

func TestInspectionBoundsExactScalarDisplayWithoutRounding(t *testing.T) {
	document, err := oif.ParseJSON([]byte(`{"temperature":0.`+strings.Repeat("0", 512)+`}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	result := inspectRequest(document, nil)
	if len(result.Fields) != 1 || result.Fields[0].Kind != "number" || !result.Fields[0].Redacted || result.Fields[0].ValueJSON != nil {
		t.Fatal("overlong numeric lexeme was exposed or silently rounded instead of remaining redacted")
	}
}

func TestInspectionExplainsOrderedToolsWithoutExposingNativeState(t *testing.T) {
	document, err := oif.ParseJSON([]byte(`{
		"system":"private-system-prompt",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"private-user-prompt"},{"type":"image","source":{"data":"private-image"}}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"private-thought","signature":"private-signature"},{"type":"text","text":"private-before"},{"type":"tool_use","id":"private-call-a","name":"private-tool-a","input":{"private-schema-key":1}},{"type":"tool_use","id":"private-call-b","name":"private-tool-b","input":{}},{"type":"text","text":"private-after"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"private-call-b","content":"private-result-b"},{"type":"tool_result","tool_use_id":"private-call-a","content":"private-result-a"}]}
		],
		"private-native-key":"private-native-value"
	}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	result := inspectRequest(document, nil)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-") {
		t.Fatalf("structural preview exposed sensitive source: %s", encoded)
	}
	if len(result.Structure) != 4 || result.Structure[0].Scope != "system" || result.Structure[1].Role != "user" || result.Structure[2].Role != "assistant" || result.Structure[3].Role != "user" {
		t.Fatalf("role/scope order lost: %+v", result.Structure)
	}
	assistant := result.Structure[2].Parts
	if len(assistant) != 5 || assistant[0].Kind != "reasoning" || assistant[1].Kind != "text" || assistant[2].Kind != "tool_call" || assistant[3].Kind != "tool_call" || assistant[4].Kind != "text" {
		t.Fatalf("assistant block order lost: %+v", assistant)
	}
	results := result.Structure[3].Parts
	if results[0].CallOrdinal == nil || *results[0].CallOrdinal != 2 || results[1].CallOrdinal == nil || *results[1].CallOrdinal != 1 {
		t.Fatalf("parallel tool correspondence lost: %+v", results)
	}
}

func TestInspectionStructureIsBoundedAndUnknownEnumsAreRedacted(t *testing.T) {
	parts := make([]string, 20)
	for index := range parts {
		parts[index] = `{"type":"private-kind","private-schema-name":"private-value"}`
	}
	document, err := oif.ParseJSON([]byte(`{"messages":[{"role":"private-role","content":[`+strings.Join(parts, ",")+`]}]}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	structure, omitted := inspectStructure(document.Root())
	if omitted != 0 || len(structure) != 1 || structure[0].Role != "other" || len(structure[0].Parts) != maxInspectedParts || structure[0].OmittedParts != 4 {
		t.Fatalf("bounded structure was not preserved: %+v, omitted %d", structure, omitted)
	}
	encoded, _ := json.Marshal(structure)
	if strings.Contains(string(encoded), "private-") {
		t.Fatalf("unknown enum or member escaped: %s", encoded)
	}
}
