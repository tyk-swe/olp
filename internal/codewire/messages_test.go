package codewire_test

import (
	"strings"
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

func TestSummaryHistoryRequiresCompletePortableConversation(t *testing.T) {
	body := []byte(`{"model":"gemini-fixture","instructions":"Keep the requirements","input":[{"role":"user","content":"Inspect the repository"},{"type":"function_call","call_id":"call-a","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call-a","output":"The project uses PostgreSQL"}]}`)
	prepared, err := codewire.SummaryHistory(body, nil)
	if err != nil || !strings.Contains(prepared, "PostgreSQL") || !strings.Contains(prepared, "Keep the requirements") {
		t.Fatalf("complete history = %q, %v", prepared, err)
	}
	for _, input := range []string{
		`{"previous_response_id":"missing","input":"hello"}`,
		`{"input":"hello","reasoning":{"effort":"high"}}`,
		`{"input":"hello","include":["foreign.output"]}`,
		`{"input":"hello","tool_choice":"required"}`,
		`{"input":"hello","tools":[{"type":"web_search"}]}`,
		`{"input":"hello","text":{"format":{"type":"json_schema"}}}`,
		`{"input":[{"type":"function_call","call_id":"call-a","name":"read","arguments":"{}"}]}`,
		`{"input":[{"type":"function_call_output","call_id":"call-a","output":"orphan"}]}`,
		`{"input":[{"type":"compaction","encrypted_content":"foreign"}]}`,
		`{"input":[{"type":"reasoning","encrypted_content":"foreign"}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"image"}]}]}`,
	} {
		if _, err := codewire.SummaryHistory([]byte(input), nil); err == nil {
			t.Fatalf("unportable history accepted %s", input)
		}
	}
}
