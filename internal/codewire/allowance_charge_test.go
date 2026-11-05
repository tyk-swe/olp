package codewire_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/codewire"
)

// Fixture shape per https://opencode.ai/v2/docs/console/api/inference/ (fetched 2026-11-05): usage.cost beside usage.opencode{billing_source,list_cost,allowance,tokens}; allowance is Go only, in dollars of the plan monthly limit.
const openCodeUsage = `"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,"cost":0.00412,"opencode":{"billing_source":"go","list_cost":0.00412,"allowance":0.0021,"tokens":{"input":1200,"output":300,"reasoning":120,"cache_read":8000,"cache_write_5m":0,"cache_write_1h":0}}}`

func decodedCharge(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	if raw == nil {
		t.Fatal("allowance charge is nil, want the reported vendor object")
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("allowance charge is not a JSON object: %v (%s)", err, raw)
	}
	return object
}

func TestChatUnaryReportsOpenCodeAllowanceCharge(t *testing.T) {
	chat := codewire.NewChat()
	observation := chat.Unary([]byte(`{"choices":[{"finish_reason":"stop"}],` + openCodeUsage + `}`))
	if !observation.Terminal || !observation.Successful {
		t.Fatalf("observation = %+v", observation)
	}
	charge := decodedCharge(t, observation.AllowanceCharge)
	if charge["billing_source"] != "go" || charge["allowance"] != 0.0021 || charge["list_cost"] != 0.00412 || charge["cost"] != 0.00412 {
		t.Fatalf("charge = %v, want the vendor object as reported plus usage.cost", charge)
	}
	tokens, ok := charge["tokens"].(map[string]any)
	if !ok || tokens["cache_read"] != float64(8000) {
		t.Fatalf("charge tokens = %v, want the reported token categories", charge["tokens"])
	}
}

func TestChatStreamReportsChargeFromFinalUsageChunk(t *testing.T) {
	chat := codewire.NewChat()
	if charge := chat.Event([]byte(`{"choices":[{"delta":{}}]}`)).AllowanceCharge; charge != nil {
		t.Fatalf("charge before final usage chunk = %s, want nil", charge)
	}
	chat.Event([]byte(`{"choices":[{"finish_reason":"stop"}],` + openCodeUsage + `}`))
	observation := chat.Event([]byte("[DONE]"))
	charge := decodedCharge(t, observation.AllowanceCharge)
	if charge["allowance"] != 0.0021 {
		t.Fatalf("charge = %v, want the final chunk allowance as reported", charge)
	}
}

func TestChatWithoutOpenCodeObjectKeepsChargeUnknown(t *testing.T) {
	chat := codewire.NewChat()
	observation := chat.Unary([]byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	if observation.AllowanceCharge != nil {
		t.Fatalf("charge = %s, want nil (unknown, never zero)", observation.AllowanceCharge)
	}
	if observation.Usage.Total == nil || *observation.Usage.Total != 5 {
		t.Fatalf("usage = %+v", observation.Usage)
	}
}

func TestMessagesUnaryReportsOpenCodeAllowanceCharge(t *testing.T) {
	messages := codewire.NewMessages()
	body := `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":1200,"output_tokens":300,"cost":0.00412,"opencode":{"billing_source":"go","list_cost":0.00412,"allowance":0.0021,"tokens":{"input":1200,"output":300}}}}`
	observation := messages.Unary([]byte(body))
	if !observation.Terminal || !observation.Successful {
		t.Fatalf("observation = %+v", observation)
	}
	charge := decodedCharge(t, observation.AllowanceCharge)
	if charge["allowance"] != 0.0021 || charge["cost"] != 0.00412 {
		t.Fatalf("charge = %v, want the vendor object as reported plus usage.cost", charge)
	}
}

func TestMessagesStreamReportsChargeFromMessageDelta(t *testing.T) {
	messages := codewire.NewMessages()
	messages.Event([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":1200,"output_tokens":1}}}`))
	messages.Event([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":300,"cost":0.00412,"opencode":{"billing_source":"go","allowance":0.0021}}}`))
	observation := messages.Event([]byte(`{"type":"message_stop"}`))
	charge := decodedCharge(t, observation.AllowanceCharge)
	if charge["allowance"] != 0.0021 {
		t.Fatalf("charge = %v, want the message_delta allowance as reported", charge)
	}
}

func TestMessagesWithoutOpenCodeObjectKeepsChargeUnknown(t *testing.T) {
	messages := codewire.NewMessages()
	observation := messages.Unary([]byte(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`))
	if observation.AllowanceCharge != nil {
		t.Fatalf("charge = %s, want nil (unknown, never zero)", observation.AllowanceCharge)
	}
}

func TestNonObjectOpenCodeValueIsIgnored(t *testing.T) {
	chat := codewire.NewChat()
	observation := chat.Unary([]byte(`{"choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"opencode":"go"}}`))
	if observation.AllowanceCharge != nil {
		t.Fatalf("charge = %s, want nil for a non-object opencode value", observation.AllowanceCharge)
	}
}
