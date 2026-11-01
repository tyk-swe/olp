package prompts

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestTemplateLanguage(t *testing.T) {
	d := Definition{Template: "Hello {{name}} / {{name}}", Variables: []string{"name"}}
	got, err := d.Render(map[string]string{"name": "{{literal}}"})
	if err != nil || got != "Hello {{literal}} / {{literal}}" {
		t.Fatalf("render interpreted a value: %q %v", got, err)
	}
	for _, bad := range []Definition{{Template: "{{name", Variables: []string{"name"}}, {Template: "{{name}} }}", Variables: []string{"name"}}, {Template: "{{name + other}}", Variables: []string{"name"}}, {Template: "{{name}}", Variables: []string{"name", "name"}}, {Template: "plain", Variables: []string{"unused"}}, {Template: strings.Repeat("é", MaxTemplateBytes/2+1)}, {Template: string([]byte{0xff})}} {
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid definition: %+v", bad)
		}
	}
	for _, vars := range []map[string]string{nil, {"unknown": "x"}, {"name": "x", "extra": "x"}, {"name": strings.Repeat("x", MaxTemplateBytes)}} {
		if _, err = d.Render(vars); err == nil {
			t.Fatal("accepted missing, unknown or oversized variables")
		}
	}
}

func TestStructuredPromptRendersRolesAndDeclaredVariables(t *testing.T) {
	definition := Definition{
		Messages: []Message{
			{Role: "system", Content: "Act as {{role}}"},
			{Role: "user", Content: "Question: {{question}}"},
			{Role: "assistant", Content: "Ready"},
		},
		VariableSchema: &VariableSchema{
			Type: "object", Properties: map[string]VariableProperty{"role": {Type: "string"}, "question": {Type: "string"}}, Required: []string{"role", "question"},
		},
		DefaultParameters: &Parameters{Temperature: new(0.7), TopP: new(0.9), MaxOutputTokens: new(int64(256))},
	}
	vars := map[string]string{"role": "a guide", "question": "{{literal}}"}
	want := []Message{{Role: "system", Content: "Act as a guide"}, {Role: "user", Content: "Question: {{literal}}"}, {Role: "assistant", Content: "Ready"}}
	limit := int64(len(want[0].Content) + len(want[1].Content) + len(want[2].Content))
	got, err := definition.RenderMessages(vars, limit)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("rendered messages = %+v, %v; want %+v", got, err, want)
	}
	cloned := definition.Clone()
	clonedMessages, err := cloned.RenderMessages(vars, limit)
	if err != nil || !reflect.DeepEqual(clonedMessages, want) || cloned.DefaultParameters == nil || *cloned.DefaultParameters.Temperature != 0.7 || *cloned.DefaultParameters.TopP != 0.9 || *cloned.DefaultParameters.MaxOutputTokens != 256 {
		t.Fatalf("cloned prompt changed roles, values or defaults: %+v, %v", cloned, err)
	}
	for _, values := range []map[string]string{nil, {"role": "a guide"}, {"role": "a guide", "question": "x", "extra": "x"}, {"role": "a guide", "question": string([]byte{0xff})}} {
		if _, err := definition.RenderMessages(values, 1024); !errors.Is(err, ErrTemplate) {
			t.Fatalf("accepted invalid variables %v: %v", values, err)
		}
	}
	if _, err := definition.RenderMessages(vars, limit-1); !errors.Is(err, ErrRenderTooLarge) {
		t.Fatalf("accepted a rendered prompt exceeding its combined limit: %v", err)
	}
}

func TestStructuredPromptRejectsInvalidRolesAndSchema(t *testing.T) {
	validSchema := &VariableSchema{Type: "object", Properties: map[string]VariableProperty{"name": {Type: "string"}}, Required: []string{"name"}}
	for _, tc := range []struct {
		name       string
		definition Definition
	}{
		{"unsupported role", Definition{Messages: []Message{{Role: "tool", Content: "{{name}}"}}, VariableSchema: validSchema}},
		{"system after user", Definition{Messages: []Message{{Role: "user", Content: "{{name}}"}, {Role: "system", Content: "instruction"}}, VariableSchema: validSchema}},
		{"unclosed variable", Definition{Messages: []Message{{Role: "user", Content: "{{name"}}, VariableSchema: validSchema}},
		{"undeclared variable", Definition{Messages: []Message{{Role: "user", Content: "{{other}}"}}, VariableSchema: validSchema}},
		{"unused variable", Definition{Messages: []Message{{Role: "user", Content: "hello"}}, VariableSchema: validSchema}},
		{"invalid message bytes", Definition{Messages: []Message{{Role: "user", Content: string([]byte{0xff}) + "{{name}}"}}, VariableSchema: validSchema}},
		{"message too large", Definition{Messages: []Message{{Role: "user", Content: strings.Repeat("x", MaxTemplateBytes) + "{{name}}"}}, VariableSchema: validSchema}},
		{"mixed template and messages", Definition{Template: "legacy", Messages: []Message{{Role: "user", Content: "{{name}}"}}, VariableSchema: validSchema}},
		{"missing schema", Definition{Messages: []Message{{Role: "user", Content: "{{name}}"}}, Variables: []string{"name"}}},
		{"schema not object", Definition{Messages: []Message{{Role: "user", Content: "{{name}}"}}, VariableSchema: &VariableSchema{Type: "array", Properties: validSchema.Properties, Required: []string{"name"}}}},
		{"additional properties", Definition{Messages: []Message{{Role: "user", Content: "{{name}}"}}, VariableSchema: &VariableSchema{Type: "object", Properties: validSchema.Properties, Required: []string{"name"}, AdditionalProperties: true}}},
		{"optional variable", Definition{Messages: []Message{{Role: "user", Content: "{{name}}"}}, VariableSchema: &VariableSchema{Type: "object", Properties: validSchema.Properties}}},
		{"unknown required variable", Definition{Messages: []Message{{Role: "user", Content: "{{name}}"}}, VariableSchema: &VariableSchema{Type: "object", Properties: validSchema.Properties, Required: []string{"other"}}}},
		{"non-string variable", Definition{Messages: []Message{{Role: "user", Content: "{{name}}"}}, VariableSchema: &VariableSchema{Type: "object", Properties: map[string]VariableProperty{"name": {Type: "number"}}, Required: []string{"name"}}}},
		{"duplicate required variable", Definition{Messages: []Message{{Role: "user", Content: "{{name}}"}}, VariableSchema: &VariableSchema{Type: "object", Properties: map[string]VariableProperty{"name": {Type: "string"}, "other": {Type: "string"}}, Required: []string{"name", "name"}}}},
		{"mixed variable declarations", Definition{Messages: []Message{{Role: "user", Content: "{{name}}"}}, Variables: []string{"name"}, VariableSchema: validSchema}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.definition.Validate(); !errors.Is(err, ErrTemplate) {
				t.Fatalf("accepted invalid definition: %v", err)
			}
		})
	}
}

func TestPromptDefaultParametersRespectBounds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parameters *Parameters
		valid      bool
	}{
		{"absent", nil, true},
		{"minimum", &Parameters{Temperature: new(0.0), TopP: new(0.0), MaxOutputTokens: new(int64(1))}, true},
		{"maximum", &Parameters{Temperature: new(2.0), TopP: new(1.0), MaxOutputTokens: new(int64(2147483647))}, true},
		{"temperature below minimum", &Parameters{Temperature: new(-0.1)}, false},
		{"temperature above maximum", &Parameters{Temperature: new(2.1)}, false},
		{"temperature not a number", &Parameters{Temperature: new(math.NaN())}, false},
		{"top p below minimum", &Parameters{TopP: new(-0.1)}, false},
		{"top p above maximum", &Parameters{TopP: new(1.1)}, false},
		{"top p not a number", &Parameters{TopP: new(math.NaN())}, false},
		{"zero tokens", &Parameters{MaxOutputTokens: new(int64(0))}, false},
		{"too many tokens", &Parameters{MaxOutputTokens: new(int64(2147483648))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (Definition{Template: "hello", DefaultParameters: tc.parameters}).Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("valid = %v, got %v", tc.valid, err)
			}
		})
	}
}
