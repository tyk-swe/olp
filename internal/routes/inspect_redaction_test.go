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

func TestInspectionFieldScopesResultAndOperationControls(t *testing.T) {
	for pointer, want := range map[string]string{
		"/dimensions":                          "/dimensions",
		"/output_dimension":                    "/output_dimension",
		"/encoding_format":                     "/encoding_format",
		"/top_n":                               "/top_n",
		"/documents":                           "/documents",
		"/result/model":                        "/result/model",
		"/result/usage/prompt_tokens":          "/result/usage/prompt_tokens",
		"/result/results/0/document":           "/result/results/0/document",
		"/result/results/0/private-name":       "/result/results",
		"/result/private-member":               "/native_fields",
		"/private-native-key":                  "/native_fields",
		"/$native":                             "/$native",
		"/result/usage/prompt~1private-marker": "/result/usage",
	} {
		if got := inspectionField(pointer); got != want {
			t.Fatalf("inspectionField(%s) = %s, want %s", pointer, got, want)
		}
	}
}

func TestInspectionTagsQualifiedProvenancePerPointer(t *testing.T) {
	document, err := oif.ParseJSON([]byte(`{
		"model":"route","input":["private-inspector-prompt"],"output_dimension":2,"truncation":false,"encoding_format":null,"private-native-key":1
	}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	provenance := []oif.Provenance{
		{Pointer: "", Origin: oif.QualifiedMapping, Reason: "qualified-openai-voyage-float-embedding/1"},
		{Pointer: "/truncation", Origin: oif.QualifiedMapping, Reason: "registered equivalent native control"},
		{Pointer: "/output_dimension", Origin: oif.QualifiedMapping, Reason: "qualified native control relocation"},
		{Pointer: "/model", Origin: oif.IdentityBinding, Reason: "published model binding"},
	}
	result := inspectRequest(document, provenance)
	fields := map[string]inspectedField{}
	for _, field := range result.Fields {
		fields[field.Field] = field
	}
	if fields["/truncation"].Origin != "qualified_mapping" || fields["/output_dimension"].Origin != "qualified_mapping" {
		t.Fatalf("qualified change provenance lost: %+v", result.Fields)
	}
	if fields["/model"].Origin != "identity_binding" {
		t.Fatalf("identity binding provenance lost: %+v", fields["/model"])
	}
	// Untouched members stay caller-owned: the root mapping marker is a class
	// claim, not per-member evidence.
	if fields["/input"].Origin != "caller" || fields["/encoding_format"].Origin != "caller" {
		t.Fatalf("untouched member mislabeled as mapped: %+v", result.Fields)
	}
	// Safe operation control values display; content and names never do.
	if field := fields["/output_dimension"]; field.Redacted || field.ValueJSON == nil || *field.ValueJSON != "2" {
		t.Fatalf("safe numeric control not shown: %+v", field)
	}
	if field := fields["/truncation"]; field.Redacted || field.ValueJSON == nil || *field.ValueJSON != "false" {
		t.Fatalf("safe boolean control not shown: %+v", field)
	}
	if !fields["/input"].Redacted || fields["/input"].ValueJSON != nil {
		t.Fatal("native input content exposed")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "private-") {
		t.Fatalf("inspection exposed caller content or names: %s", encoded)
	}
	if result.RedactedNativeFields != 1 {
		t.Fatal("unknown native field was not counted without its name")
	}
}

func TestInspectionOperationControlValuesStayEnumBounded(t *testing.T) {
	for pointer, raw := range map[string]string{
		"/output_dtype": `"uint8"`,
		"/input_type":   `"query"`,
		"/truncate":     `"END"`,
		"/taskType":     `"RETRIEVAL_QUERY"`,
	} {
		document, _ := oif.ParseJSON([]byte(`{"x":`+raw+`}`), oif.Limits{})
		value, _ := document.Root().Lookup("x")
		if !safeInspectionValue(pointer, value) {
			t.Fatalf("registered enum value redacted at %s", pointer)
		}
	}
	for pointer, raw := range map[string]string{
		"/output_dtype": `"private-dtype-marker"`,
		"/prompt_name":  `"private-prompt-marker"`,
		"/title":        `"private-title-marker"`,
	} {
		document, _ := oif.ParseJSON([]byte(`{"x":`+raw+`}`), oif.Limits{})
		value, _ := document.Root().Lookup("x")
		if safeInspectionValue(pointer, value) {
			t.Fatalf("caller-controlled string exposed at %s", pointer)
		}
	}
}
