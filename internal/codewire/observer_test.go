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

func TestGenericClientRejectsAmbiguousIdentity(t *testing.T) {
	for _, headers := range []http.Header{
		{"X-Olp-Code-Affinity": {"one", "two"}},
		{"X-Olp-Code-Affinity": {""}},
		{"X-Olp-Code-Affinity": {" one"}},
		{"X-Olp-Code-Affinity": {"one"}, "X-Claude-Code-Session-Id": {"session"}},
		{"X-Olp-Code-Affinity": {"one"}, "X-Opencode-Session-Id": {"session"}},
		{"Thread-Id": {"thread"}},
		{"X-Claude-Code-Session-Id": {"session"}, "Thread-Id": {"thread"}},
	} {
		if _, err := ClassifyMessages([]byte(`{"model":"native-model","messages":[]}`), headers); err == nil {
			t.Fatalf("ambiguous identity accepted %+v", headers)
		}
	}
}

func TestGenericMessagesRejectsOpaqueState(t *testing.T) {
	for _, body := range []string{
		`{"model":"native-model","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret","signature":"opaque"}]}]}`,
		`{"model":"native-model","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`,
		`{"model":"native-model","messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"unowned"}}]}]}`,
		`{"model":"native-model","messages":[],"context_management":{"edits":[{"type":"clear_tool_uses"}]}}`,
	} {
		if _, err := ClassifyMessages([]byte(body), http.Header{"X-Olp-Code-Affinity": {"conversation"}}); err == nil {
			t.Fatalf("opaque state accepted %s", body)
		}
	}
}
