package codewire_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/codemode/codewire"
)

func TestMessagesUnarySurfacesBareErrorDocument(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), errors are a single error object with code and message, with no type field.
	body := []byte(`{"error":{"code":"1308","message":"Usage limit reached for 5 hours. Your limit will reset at 2026-11-05T12:00:00Z"}}`)
	got := codewire.NewMessages().Unary(body)
	if string(got.RejectionBody) != string(body) || !got.Terminal {
		t.Fatalf("Unary(bare error) = %+v", got)
	}
	success := codewire.NewMessages().Unary([]byte(`{"id":"msg","type":"message","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	if len(success.RejectionBody) != 0 {
		t.Fatalf("Unary(success) rejection body = %q", success.RejectionBody)
	}
}

func TestMessagesUsageEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, usage, want string }{
		{"cache categories are added to input", `{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":6}`, `{"Total":24,"Input":19,"Output":5,"Cached":3,"Reasoning":null}`},
		{"without cache categories", `{"input_tokens":10,"output_tokens":5}`, `{"Total":15,"Input":10,"Output":5,"Cached":null,"Reasoning":null}`},
		{"output not reported", `{"input_tokens":10}`, noUsage},
		{"input not reported", `{"output_tokens":5}`, noUsage},
		{"negative count", `{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":-1}`, noUsage},
		{"inexact sum", `{"input_tokens":9007199254740991,"output_tokens":1}`, noUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := codewire.NewMessages().Unary([]byte(`{"id":"msg","type":"message","stop_reason":"end_turn","usage":` + tc.usage + `}`))
			assertUsage(t, got.Usage, tc.want)
		})
	}
}

func TestMessagesStreamUsageNeedsTheFinalOutputCount(t *testing.T) {
	t.Parallel()
	const start = `{"type":"message_start","message":{"id":"msg","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":3}}}`
	for _, tc := range []struct{ name, delta, want string }{
		{"final output", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`, `{"Total":22,"Input":13,"Output":9,"Cached":3,"Reasoning":null}`},
		{"final output missing", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, noUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := codewire.NewMessages()
			messages.Event([]byte(start))
			messages.Event([]byte(tc.delta))
			assertUsage(t, messages.Event([]byte(`{"type":"message_stop"}`)).Usage, tc.want)
		})
	}
}
