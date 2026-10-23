package retainedapi

import "testing"

func TestSupportsRM5Matrix(t *testing.T) {
	cases := []struct {
		kind, operation, surface, mode string
		want                           bool
	}{
		{"openai", "fine_tuning", "openai", "unary", true},
		{"azure_openai", "fine_tuning", "openai", "unary", true},
		{"vertex_ai", "fine_tuning", "native", "unary", true},
		{"openai", "vector_store", "openai", "unary", true},
		{"azure_openai", "retrieval", "openai", "unary", true},
		{"bedrock", "retrieval", "openai", "unary", true},
		{"openai", "conversation", "openai", "unary", true},
		{"azure_openai", "conversation", "openai", "unary", false},
		{"openai", "eval", "openai", "unary", true},
		{"anthropic", "skill", "anthropic", "unary", true},
		{"gemini", "context_cache", "gemini", "unary", true},
		{"gemini", "agent_resource", "gemini", "unary", true},
		{"openai", "fine_tuning", "openai", "streaming", false},
		{"anthropic", "fine_tuning", "openai", "unary", false},
		{"openai", "skill", "anthropic", "unary", false},
		{"openai_compatible", "vector_store", "openai", "unary", false},
	}
	for _, tc := range cases {
		if got := Supports(tc.kind, tc.operation, tc.surface, tc.mode); got != tc.want {
			t.Errorf("Supports(%q,%q,%q,%q)=%v want %v", tc.kind, tc.operation, tc.surface, tc.mode, got, tc.want)
		}
	}
}

func TestEndpointsCoverRM5Families(t *testing.T) {
	ops := map[string]bool{}
	for _, e := range Endpoints() {
		ops[e.Operation] = true
		if !Operation(e.Operation) {
			t.Fatalf("endpoint operation not declared: %s", e.Operation)
		}
	}
	for _, op := range []string{"fine_tuning", "vector_store", "conversation", "container", "eval", "skill", "context_cache", "agent_resource"} {
		if !ops[op] {
			t.Fatalf("missing endpoints for %s", op)
		}
	}
}
