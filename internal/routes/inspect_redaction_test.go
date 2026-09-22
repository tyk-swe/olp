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
