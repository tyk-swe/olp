package codewire

import (
	"net/http"
	"testing"
)

func TestMessagesUsageIncludesCacheInInput(t *testing.T) {
	observer := NewMessages()
	observer.Event([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"output_tokens":1}}}`))
	observer.Event([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`))
	observation := observer.Event([]byte(`{"type":"message_stop"}`))
	if !observation.Terminal || !observation.Successful {
		t.Fatalf("stream did not complete: %+v", observation)
	}
	usage := observation.Usage
	if usage.Total == nil || *usage.Total != 20 || usage.Input == nil || *usage.Input != 15 || usage.Cached == nil || *usage.Cached != 3 {
		t.Fatalf("cache tokens not folded into input: %+v", usage)
	}
}

func TestChatStreamWithoutUsageLeavesConsumptionUnknown(t *testing.T) {
	observer := NewChat()
	observer.Event([]byte(`{"choices":[{"finish_reason":"stop"}]}`))
	observation := observer.Event([]byte("[DONE]"))
	if !observation.Terminal {
		t.Fatalf("stream did not finish: %+v", observation)
	}
	if observation.Usage.Total != nil {
		t.Fatalf("missing usage reported as a total: %+v", observation.Usage)
	}
}

func TestClientIdentityScopesClaudeAgentsToSession(t *testing.T) {
	headers := http.Header{
		"X-Claude-Code-Session-Id": {"ses_1"},
		"X-Claude-Code-Agent-Id":   {"reviewer"},
	}
	identity, err := ClientIdentity(headers)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Conversation != "ses:5f1/reviewer" || identity.Parent != "ses:5f1" {
		t.Fatalf("agent not scoped by session: %+v", identity)
	}
	headers.Set("X-Opencode-Session-Id", "other")
	if _, err := ClientIdentity(headers); err == nil {
		t.Fatal("request naming both clients accepted")
	}
}

func TestClassifyChatRejectsMissingModel(t *testing.T) {
	headers := http.Header{"X-Opencode-Session-Id": {"ses-1"}}
	if _, err := ClassifyChat([]byte(`{"messages":[]}`), headers); err == nil {
		t.Fatal("request without a model classified")
	}
	request, err := ClassifyChat([]byte(`{"model":"glm-5.3","max_tokens":7}`), headers)
	if err != nil {
		t.Fatal(err)
	}
	if request.Operation.Model != "glm-5.3" || request.Estimate < 7 {
		t.Fatalf("classified request: %+v", request)
	}
}
