package codewire_test

import (
	"testing"

	"github.com/tyk-swe/olp/internal/codewire"
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
