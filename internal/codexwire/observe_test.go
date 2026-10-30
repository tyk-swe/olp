package codexwire

import (
	"net/http"
	"testing"
)

func TestClassifyRejectsBackgroundGenerations(t *testing.T) {
	headers := http.Header{"Thread-Id": {"thread-1"}}
	if _, err := Classify([]byte(`{"model":"gpt-5.5","background":true}`), headers, "responses", false); err == nil {
		t.Fatal("background generation classified")
	}
	request, err := Classify([]byte(`{"model":"gpt-5.5","type":"response.create","generate":false}`), headers, "responses", true)
	if err != nil {
		t.Fatal(err)
	}
	if request.Operation.Name != "prewarm" || request.Operation.Identity.Conversation != "thread-1" {
		t.Fatalf("prewarm request: %+v", request)
	}
}

func TestClassifyRequiresParentForSubagents(t *testing.T) {
	headers := http.Header{"Thread-Id": {"child-1"}, "X-Openai-Subagent": {"review"}}
	if _, err := Classify([]byte(`{"model":"gpt-5.5"}`), headers, "responses", false); err == nil {
		t.Fatal("subagent without a parent classified")
	}
}

func TestGenericResponsesCannotClaimRecognizedLineage(t *testing.T) {
	for _, headers := range []http.Header{
		{"X-Olp-Code-Affinity": {"conversation"}, "Thread-Id": {"thread"}},
		{"X-Olp-Code-Affinity": {"conversation"}, "Session_id": {"session"}},
		{"X-Claude-Code-Session-Id": {"session"}},
		{"Thread-Id": {"olp:affinity/conversation"}},
		{"Thread-Id": {"child"}, "X-Codex-Parent-Thread-Id": {"olp:request/request"}},
	} {
		if _, err := Classify([]byte(`{"model":"native-model","input":"hello"}`), headers, "responses", false); err == nil {
			t.Fatalf("mixed or reserved identity accepted %+v", headers)
		}
	}
	if _, err := Classify([]byte(`{"model":"native-model","client_metadata":{"thread_id":"thread"}}`), http.Header{"X-Olp-Code-Affinity": {"conversation"}}, "responses", false); err == nil {
		t.Fatal("body identity accepted with generic affinity")
	}
}

func TestObserveReadsTerminalUsageAsSubsets(t *testing.T) {
	observation := Observe([]byte(`{"type":"response.completed","response":{"id":"resp-1","usage":{"total_tokens":10,"input_tokens":6,"output_tokens":4,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":3}}}}`), false)
	if !observation.Terminal || !observation.Successful || observation.ResponseID != "resp-1" {
		t.Fatalf("terminal event: %+v", observation)
	}
	if observation.Usage.Total == nil || *observation.Usage.Total != 10 || observation.Usage.Cached == nil || *observation.Usage.Cached != 2 {
		t.Fatalf("usage: %+v", observation.Usage)
	}
	incomplete := Observe([]byte(`{"type":"response.created","response":{"id":"resp-2"}}`), false)
	if incomplete.Terminal {
		t.Fatal("non-terminal event settled as terminal")
	}
}
