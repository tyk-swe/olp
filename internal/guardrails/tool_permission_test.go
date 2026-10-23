package guardrails

import (
	"context"
	"encoding/json"
	"testing"
)

func TestToolPermissionOrderedRules(t *testing.T) {
	raw := json.RawMessage(`{"rules":[{"id":"read","tool_name":"read_.*","tool_type":"function","decision":"allow","allowed_param_patterns":{"path":"/safe/[^.]+","options.scopes[]":"read|list"}},{"id":"deny-rest","tool_name":".*","decision":"deny"}]}`)
	detector, err := compileToolPermission(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, tool, kind, args string
		blocked                bool
	}{
		{"allow", "read_file", "tool_call", `{"path":"/safe/file","options":{"scopes":["read","list"]}}`, false},
		{"deny-path", "read_file", "tool_call", `{"path":"/etc/passwd","options":{"scopes":["read"]}}`, true},
		{"deny-array-item", "read_file", "tool_call", `{"path":"/safe/file","options":{"scopes":["read","write"]}}`, true},
		{"missing-parameter", "read_file", "tool_call", `{"path":"/safe/file"}`, true},
		{"literal-path", "read_file", "tool_call", `{"path":"/safe/file","options.scopes[]":"read"}`, true},
		{"literal-array", "read_file", "tool_call", `{"path":"/safe/file","options":{"scopes[]":"read"}}`, true},
		{"literal-with-unsafe-nested-value", "read_file", "tool_call", `{"path":"/safe/file","options.scopes[]":"read","options":{"scopes":["write"]}}`, true},
		{"malformed-arguments", "read_file", "tool_call", `not-json`, true},
		{"nonobject-arguments", "read_file", "tool_call", `[]`, true},
		{"definition-skips-arguments", "read_file", "tool_definition", `{"type":"object"}`, false},
		{"deny-name", "execute", "tool_call", `{}`, true},
		{"fullmatch-name", "prefix_read_file", "tool_call", `{}`, true},
		{"ignore-tool-results", "execute", "tool_result", `{}`, false},
		{"ignore-messages", "execute", "text", `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := detector.detect(t.Context(), Evaluation{Segments: []Segment{{ID: "tool", Kind: tc.kind, ToolName: tc.tool, Text: tc.args}}})
			if err != nil || (result.Action == Block) != tc.blocked {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
	if detector.width() != 0 {
		t.Fatal("tool permission must require complete content")
	}
}

func TestToolPermissionDefaultsAndRuleFallback(t *testing.T) {
	for _, tc := range []struct {
		config, args string
		blocked      bool
	}{
		{`{}`, `{}`, true},
		{`{"default_action":"allow"}`, `{}`, false},
		{`{"default_action":"allow","rules":[{"id":"deny","tool_name":"danger","decision":"deny"}]}`, `{}`, true},
		{`{"default_action":"deny","rules":[{"id":"first","tool_name":"danger","decision":"allow"},{"id":"second","tool_name":"danger","decision":"deny"}]}`, `{}`, false},
		{`{"default_action":"allow","rules":[{"id":"conditional","tool_name":"danger","decision":"deny","allowed_param_patterns":{"x":"yes"}}]}`, `{"x":"no"}`, false},
		{`{"default_action":"allow","rules":[{"id":"conditional","tool_name":"danger","decision":"allow","allowed_param_patterns":{"x":"yes"}}]}`, `{}`, true},
		{`{"default_action":"allow","rules":[{"id":"conditional","tool_name":"danger","decision":"allow","allowed_param_patterns":{"x":"yes"}}]}`, `bad-json`, true},
		{`{"default_action":"deny","rules":[{"id":"type","tool_type":"function","decision":"allow"}]}`, `{}`, false},
	} {
		detector, err := compileToolPermission(json.RawMessage(tc.config))
		if err != nil {
			t.Fatal(err)
		}
		result, err := detector.detect(t.Context(), Evaluation{Segments: []Segment{{Kind: "tool_call", ToolName: "danger", Text: tc.args}}})
		if err != nil || (result.Action == Block) != tc.blocked {
			t.Fatalf("config=%s args=%s result=%+v error=%v", tc.config, tc.args, result, err)
		}
	}
}

func TestToolPermissionInvalidConfig(t *testing.T) {
	for _, raw := range []string{
		`{"default_action":"ignore"}`, `{"rules":[{"id":"x","decision":"maybe"}]}`,
		`{"rules":[{"id":"x","decision":"allow","tool_name":"["}]}`,
		`{"rules":[{"id":"x","decision":"allow","tool_type":"["}]}`,
		`{"rules":[{"id":"x","decision":"allow","allowed_param_patterns":{"path":".*"}}]}`,
		`{"rules":[{"id":"x","decision":"allow","tool_name":".*","allowed_param_patterns":{"path":"["}}]}`,
		`{"rules":[{"id":"x","decision":"allow"},{"id":"x","decision":"deny"}]}`,
		`{"rules":[{"decision":"allow"}]}`, `{"on_disallowed_action":"rewrite"}`,
	} {
		if _, err := compileToolPermission(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestToolPermissionArgumentsAreBounded(t *testing.T) {
	values, err := toolPermissionValues(`{"nested":[{"values":[1,true,null]},{"values":["text"]}]}`)
	if err != nil || len(values["nested[].values[]"]) != 4 {
		t.Fatalf("values=%v error=%v", values, err)
	}
	for _, raw := range []string{`null`, `[]`, `{} {}`, `{} broken`} {
		if _, err := toolPermissionValues(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	detector, err := compileToolPermission(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := detector.detect(ctx, Evaluation{Segments: []Segment{{Kind: "tool_call"}}}); err == nil {
		t.Fatal("ignored cancelled context")
	}
}
