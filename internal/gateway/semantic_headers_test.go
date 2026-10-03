package gateway

import (
	"net/http"
	"slices"
	"testing"
)

func TestSemanticHeadersFoldTheLinesOfAnAnthropicBetaList(t *testing.T) {
	source := http.Header{
		"Anthropic-Beta":    {"interleaved-thinking-2025-05-14", "fine-grained-tool-streaming-2025-05-14"},
		"Anthropic-Version": {"2023-06-01", "2024-01-01"},
		"Authorization":     {"Bearer a", "Bearer b"},
	}
	got := semanticHeaders(source)
	if want := []string{"interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14"}; !slices.Equal(got["Anthropic-Beta"], want) {
		t.Errorf("Anthropic-Beta is %q, want %q", got["Anthropic-Beta"], want)
	}
	// Only the beta list folds: a repeated version stays ambiguous for the
	// binder to refuse.
	if !slices.Equal(got["Anthropic-Version"], source["Anthropic-Version"]) || !slices.Equal(got["Authorization"], source["Authorization"]) {
		t.Errorf("other headers changed: %v", got)
	}
	if len(source["Anthropic-Beta"]) != 2 {
		t.Errorf("the request's own headers changed: %v", source)
	}
}

func TestSemanticHeadersLeaveASingleLineAlone(t *testing.T) {
	for _, source := range []http.Header{
		{},
		{"Anthropic-Beta": {"one,two"}},
		{"Anthropic-Beta": {"one"}, "Openai-Beta": {"assistants=v2"}},
	} {
		got := semanticHeaders(source)
		if len(got) != len(source) {
			t.Fatalf("%v became %v", source, got)
		}
		for name, values := range source {
			if !slices.Equal(got[name], values) {
				t.Errorf("%s: %q became %q", name, values, got[name])
			}
		}
	}
}
