package codexwire

import (
	"errors"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
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

func TestClassifyRejectsStoredItemReferences(t *testing.T) {
	for _, item := range []string{`{"id":"stored-item"}`, `{"type":null,"id":"stored-item"}`, `{"type":"item_reference","id":"stored-item"}`} {
		for _, headers := range []http.Header{{}, {"X-Olp-Code-Affinity": {"conversation"}}, {"Thread-Id": {"thread"}}} {
			if _, err := Classify([]byte(`{"model":"native-model","input":[`+item+`]}`), headers, "responses", false); err == nil {
				t.Fatalf("stored reference accepted %s", item)
			}
		}
	}
}

func TestResponsesRecoveryKeepsPreviousResponseOnItsAccount(t *testing.T) {
	for _, test := range []struct {
		name, body, previous string
		portable             bool
	}{
		{"initial", `{"model":"native","input":"Hello"}`, "", true},
		{"optional nulls", `{"model":"native","input":"Hello","previous_response_id":null,"conversation":null,"instructions":null}`, "", true},
		{"null input", `{"model":"native","input":null}`, "", false},
		{"previous response", `{"model":"native","input":"Continue","previous_response_id":"response-owned"}`, "response-owned", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{"X-Olp-Code-Affinity": {"conversation"}}
			request, err := Classify([]byte(test.body), headers, "responses", false)
			if err != nil || request.PreviousResponse != test.previous {
				t.Fatalf("continuation %q, want %q, error %v", request.PreviousResponse, test.previous, err)
			}
			if portable := codewire.PortableInitialRequest([]byte(test.body), headers, request); portable != test.portable {
				t.Fatalf("portable %t, want %t", portable, test.portable)
			}
		})
	}
	_, err := Classify([]byte(`{"model":"native","input":"Continue","previous_response_id":"response-owned"}`), nil, "responses", false)
	var refusal *codemode.Refusal
	if !errors.As(err, &refusal) || refusal.Code != "code_parent_unresolved" || refusal.Status != http.StatusConflict {
		t.Fatalf("unowned previous response was accepted %v", err)
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
