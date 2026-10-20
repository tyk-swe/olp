package wire

import "testing"

func TestResourceFormatOperationAndSurface(t *testing.T) {
	tests := []struct {
		format    Format
		operation string
		surface   string
	}{
		{AnthropicFile, "file", "anthropic"},
		{AnthropicBatch, "batch", "anthropic"},
		{GeminiFile, "file", "gemini"},
		{GeminiBatch, "batch", "gemini"},
		{CloudBatch, "batch", "native"},
	}
	for _, test := range tests {
		if operation := test.format.Operation(); operation != test.operation {
			t.Errorf("%s operation = %q, want %q", test.format, operation, test.operation)
		}
		if surface := test.format.Surface(); surface != test.surface {
			t.Errorf("%s surface = %q, want %q", test.format, surface, test.surface)
		}
	}
}
