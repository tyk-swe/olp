package codewire

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

func TestGenericIdentitySeparatesNativeClientFamilies(t *testing.T) {
	for _, test := range []struct {
		name    string
		family  IdentityFamily
		body    string
		headers http.Header
		generic bool
		refusal string
	}{
		{"codex absent metadata", CodexIdentity, `{}`, nil, true, ""},
		{"codex null metadata", CodexIdentity, `{"client_metadata":null}`, nil, true, ""},
		{"codex object metadata", CodexIdentity, `{"client_metadata":{}}`, nil, true, ""},
		{"codex invalid metadata", CodexIdentity, `{"client_metadata":[]}`, nil, false, "code_identity_invalid"},
		{"generic application metadata", SessionIdentity, `{"client_metadata":"application value"}`, nil, true, ""},
		{"codex native metadata", CodexIdentity, `{"client_metadata":{"thread_id":"thread"}}`, nil, false, ""},
		{"generic application thread", SessionIdentity, `{"client_metadata":{"thread_id":"thread"}}`, nil, true, ""},
		{"codex native header", CodexIdentity, `{}`, http.Header{"Thread-Id": {"thread"}}, false, ""},
		{"codex Claude header", CodexIdentity, `{}`, http.Header{"X-Claude-Code-Session-Id": {"session"}}, false, "code_identity_ambiguous"},
		{"codex OpenCode header", CodexIdentity, `{}`, http.Header{"X-Opencode-Session-Id": {"session"}}, false, "code_identity_ambiguous"},
		{"codex mixed native families", CodexIdentity, `{}`, http.Header{"Thread-Id": {"thread"}, "X-Opencode-Session-Id": {"session"}}, false, "code_identity_ambiguous"},
		{"codex metadata with affinity", CodexIdentity, `{"client_metadata":{"thread_id":"thread"}}`, http.Header{"X-Olp-Code-Affinity": {"affinity"}}, false, "code_identity_ambiguous"},
	} {
		t.Run(test.name, func(t *testing.T) {
			document, err := oif.ParseJSON([]byte(test.body), oif.Limits{MaxBytes: MaxBody})
			if err != nil {
				t.Fatal(err)
			}
			identity, generic, err := GenericIdentity(test.headers, document.Root(), test.family)
			if test.refusal != "" {
				var refusal *codemode.Refusal
				if !errors.As(err, &refusal) || refusal.Code != test.refusal || refusal.Status != 400 {
					t.Fatalf("identity refusal %v", err)
				}
				return
			}
			if err != nil || generic != test.generic || generic && (!strings.HasPrefix(identity.Conversation, "olp:request/") || identity.Parent != "") {
				t.Fatalf("identity %+v, generic %t, error %v", identity, generic, err)
			}
		})
	}
}

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

func TestModelRewritingChangesOnlyProtocolModelFields(t *testing.T) {
	body := []byte(`{ "mo\u0064el" : "native-a", "response":{"model":"native-a","output":[{"model":"native-a"}]}, "message":{"model":"native-a"},"unknown": { "model":"native-a", "number":1.2300 } }`)
	rewritten, err := RewriteResponseModel(body, "native-a", "team-code")
	var public struct {
		Model             string
		Response, Message struct{ Model string }
	}
	if err := json.Unmarshal(rewritten, &public); err != nil || public.Model != "team-code" || public.Response.Model != "native-a" || public.Message.Model != "native-a" {
		t.Fatalf("protocol model identifiers were not rewritten consistently %+v, %v", public, err)
	}
	if err != nil || !bytes.Equal(rewritten, bytes.Replace(body, []byte(`"native-a"`), []byte(`"team-code"`), 1)) || !bytes.Contains(rewritten, []byte(`"output":[{"model":"native-a"}]`)) || !bytes.Contains(rewritten, []byte(`{ "model":"native-a", "number":1.2300 }`)) {
		t.Fatalf("model rewrite changed unrelated content %s, %v", rewritten, err)
	}
	request, err := RewriteRequestModel(body, "native-a", "team-code")
	if err != nil || !bytes.Contains(request, []byte(`"response":{"model":"native-a","output":[{"model":"native-a"}]}`)) {
		t.Fatalf("request rewrite changed unrelated response metadata %s, %v", request, err)
	}
	unchanged, err := RewriteResponseModel(body, "different-model", "team-code")
	if err != nil || !bytes.Equal(unchanged, body) {
		t.Fatalf("unrelated model rewritten %s, %v", unchanged, err)
	}
	if _, err := RewriteResponseModel([]byte(`{"model":"native-a","model":"native-b"}`), "native-a", "team-code"); err == nil {
		t.Fatal("ambiguous request model rewritten")
	}
}

func TestEventModelRewritingUsesOnlyKnownProtocolEnvelopes(t *testing.T) {
	for _, test := range []struct {
		protocol        codemode.Protocol
		input, expected string
	}{
		{codemode.ProtocolResponses, `{ "type":"response.completed", "response": {"model" : "native"}, "message":{"model":"native"} }`, `{ "type":"response.completed", "response": {"model" : "alias"}, "message":{"model":"native"} }`},
		{codemode.ProtocolMessages, `{ "type":"message_start", "message": {"model" : "native"}, "response":{"model":"native"} }`, `{ "type":"message_start", "message": {"model" : "alias"}, "response":{"model":"native"} }`},
		{codemode.ProtocolChat, `{ "model" : "native", "message":{"model":"native"} }`, `{ "model" : "alias", "message":{"model":"native"} }`},
		{codemode.ProtocolResponses, `{"type":"vendor.future","response":{"model":"native"},"message":{"model":"native"}}`, `{"type":"vendor.future","response":{"model":"native"},"message":{"model":"native"}}`},
		{codemode.ProtocolMessages, `{"type":"vendor.future","response":{"model":"native"},"message":{"model":"native"}}`, `{"type":"vendor.future","response":{"model":"native"},"message":{"model":"native"}}`},
	} {
		actual, err := RewriteEventModel([]byte(test.input), test.protocol, "native", "alias")
		if err != nil || string(actual) != test.expected {
			t.Fatalf("%s %s, %v", test.protocol, actual, err)
		}
	}
}

func TestGenericClientRejectsAmbiguousIdentity(t *testing.T) {
	for _, headers := range []http.Header{
		{"X-Olp-Code-Affinity": {"one", "two"}},
		{"X-Olp-Code-Affinity": {""}},
		{"X-Olp-Code-Affinity": {" one"}},
		{"X-Olp-Code-Affinity": {strings.Repeat("a", 257)}},
		{"X-Olp-Code-Affinity": {"one"}, "X-Claude-Code-Session-Id": {"session"}},
		{"X-Olp-Code-Affinity": {"one"}, "X-Opencode-Session-Id": {"session"}},
		{"Thread-Id": {"thread"}},
		{"X-Claude-Code-Session-Id": {"session"}, "Thread-Id": {"thread"}},
		{"X-Claude-Code-Session-Id": {"session"}, "X-Opencode-Session-Id": {"session"}},
	} {
		if _, err := ClassifyMessages([]byte(`{"model":"native-model","messages":[]}`), headers); err == nil {
			t.Fatalf("ambiguous identity accepted %+v", headers)
		}
	}
}

func TestGenericProtocolsKeepRequestsIndependentUnlessAffinityIsExplicit(t *testing.T) {
	for _, test := range []struct {
		protocol codemode.Protocol
		classify func([]byte, http.Header) (codemode.Request, error)
		body     string
	}{
		{codemode.ProtocolMessages, ClassifyMessages, `{"model":"native-model","max_tokens":7,"messages":[{"role":"user","content":[{"type":"text","text":"Inspect this image"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]},{"role":"assistant","content":[{"type":"tool_use","id":"call-local","name":"inspect","input":{"signature":"application-data"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-local","content":[{"type":"text","text":"Complete"}]}]}],"tools":[{"name":"inspect","input_schema":{"type":"object"}}],"client_metadata":{"thread_id":"application-thread"},"input":{"type":"native-extension"}}`},
		{codemode.ProtocolChat, ClassifyChat, `{"model":"native-model","max_completion_tokens":7,"messages":[{"role":"user","content":"Inspect this function"},{"role":"assistant","tool_calls":[{"id":"call-local","type":"function","function":{"name":"inspect","arguments":"{\"signature\":\"application-data\"}"}}]},{"role":"tool","tool_call_id":"call-local","content":"Complete"}],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object"}}}],"client_metadata":{"thread_id":"application-thread"},"input":{"type":"native-extension"}}`},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			first, firstErr := test.classify([]byte(test.body), nil)
			second, secondErr := test.classify([]byte(test.body), nil)
			if firstErr != nil || secondErr != nil || !first.Operation.Identity.Generic() || !second.Operation.Identity.Generic() || first.Operation.Identity.Conversation == second.Operation.Identity.Conversation {
				t.Fatalf("independent generic requests shared identity or refused, first %+v error %v, second %+v error %v", first, firstErr, second, secondErr)
			}
			affinity := http.Header{"X-Olp-Code-Affinity": {strings.Repeat("a", 256)}}
			bound, boundErr := test.classify([]byte(test.body), affinity)
			continued, continuedErr := test.classify([]byte(test.body), affinity)
			other, otherErr := test.classify([]byte(test.body), http.Header{"X-Olp-Code-Affinity": {"another-conversation"}})
			if boundErr != nil || continuedErr != nil || otherErr != nil || !bound.Operation.Identity.Generic() || bound.Operation.Identity != continued.Operation.Identity || bound.Operation.Identity.Conversation == other.Operation.Identity.Conversation || bound.Operation.Identity.Conversation == first.Operation.Identity.Conversation {
				t.Fatalf("explicit affinity lost conversation scope, bound %+v error %v, continued %+v error %v, other %+v error %v", bound, boundErr, continued, continuedErr, other, otherErr)
			}
			if bound.Operation.Identity.Parent != "" || bound.Operation.Model != "native-model" || bound.Operation.Name != string(test.protocol) || bound.Estimate < 7 {
				t.Fatalf("generic classification changed native model, lineage or rate estimate %+v", bound)
			}
		})
	}
}

func TestGenericMessagesRejectsOpaqueState(t *testing.T) {
	for _, body := range []string{
		`{"model":"native-model","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"secret","signature":"opaque"}]}]}`,
		`{"model":"native-model","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`,
		`{"model":"native-model","messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"unowned"}}]}]}`,
		`{"model":"native-model","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://image.invalid/a.png","file_id":"unowned"}}]}]}`,
		`{"model":"native-model","messages":[],"context_management":{"edits":[{"type":"clear_tool_uses"}]}}`,
		`{"model":"native-model","messages":[],"previous_response_id":"unowned-response"}`,
		`{"model":"native-model","messages":[],"conversation":"unowned-conversation"}`,
		`{"model":"native-model","messages":[],"container":"unowned-container"}`,
		`{"model":"native-model","messages":[],"tools":[{"type":"web_search"}]}`,
		`{"model":"native-model","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-local","content":[{"type":"document","source":{"type":"file","file_id":"unowned"}}]}]}]}`,
	} {
		for _, headers := range []http.Header{nil, {"X-Olp-Code-Affinity": {"conversation"}}} {
			_, err := ClassifyMessages([]byte(body), headers)
			var refusal *codemode.Refusal
			if !errors.As(err, &refusal) || refusal.Status != http.StatusConflict || refusal.Code != "code_parent_unresolved" {
				t.Fatalf("opaque state did not refuse as unresolved, headers %+v body %s error %v", headers, body, err)
			}
		}
	}
}

func TestChatRecoveryRequiresPortableInitialState(t *testing.T) {
	for _, test := range []struct {
		body     string
		portable bool
	}{
		{`{"model":"native","messages":[{"role":"system","content":"Keep this"},{"role":"user","content":"Hello"}],"reasoning_effort":"high","tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"read_file"}},"max_completion_tokens":64}`, true},
		{`{"model":"native","messages":[{"role":"assistant","content":"Earlier reply"},{"role":"user","content":"Continue"}]}`, false},
		{`{"model":"native","messages":[{"role":"user","content":"Hello"}],"previous_response_id":"owned"}`, false},
		{`{"model":"native","messages":[{"role":"user","content":"Hello"}],"tools":[{"type":"function","function":{"name":"read_file","signed_state":"required"}}]}`, false},
	} {
		request, err := ClassifyChat([]byte(test.body), http.Header{"X-Opencode-Session-Id": {"portable-chat"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := PortableInitialRequest([]byte(test.body), nil, request); got != test.portable {
			t.Fatalf("portable %t, want %t, body %s", got, test.portable, test.body)
		}
	}
}

func TestMessagesRecoveryRequiresPortableInitialState(t *testing.T) {
	conversation := http.Header{"X-Opencode-Session-Id": {"conversation"}}
	for _, test := range []struct {
		name, body string
		headers    http.Header
		portable   bool
	}{
		{"optional nulls", `{"model":"native","messages":[{"role":"user","content":"Hello"}],"system":null,"temperature":null,"metadata":null}`, conversation, true},
		{"function tools", `{"model":"native","messages":[{"role":"user","content":"Read main.go"}],"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object"},"eager_input_streaming":true}],"tool_choice":{"type":"tool","name":"read_file","disable_parallel_tool_use":true}}`, conversation, true},
		{"server tools", `{"model":"native","messages":[],"tools":[{"type":"web_search","name":"web_search"}]}`, conversation, false},
		{"signed tool state", `{"model":"native","messages":[],"tools":[{"name":"read_file","input_schema":{"type":"object"},"signed_state":"required"}]}`, conversation, false},
		{"malformed tool schema", `{"model":"native","messages":[],"tools":[{"name":"read_file","input_schema":"opaque"}]}`, conversation, false},
		{"unknown tool choice", `{"model":"native","messages":[],"tool_choice":{"type":"server","state":"required"}}`, conversation, false},
		{"null messages", `{"model":"native","messages":null}`, conversation, false},
		{"account continuation", `{"model":"native","messages":[]}`, http.Header{"X-Opencode-Session-Id": {"child"}, "X-Opencode-Parent-Session-Id": {"parent"}}, false},
		{"opaque turn state", `{"model":"native","messages":[]}`, http.Header{"X-Opencode-Session-Id": {"conversation"}, "X-Codex-Turn-State": {"opaque"}}, false},
		{"assistant history", `{"model":"native","messages":[{"role":"assistant","content":"Earlier reply"}]}`, conversation, false},
		{"unknown policy", `{"model":"native","messages":[],"required_feature":true}`, conversation, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := ClassifyMessages([]byte(test.body), test.headers)
			if err != nil {
				t.Fatal(err)
			}
			if got := PortableInitialRequest([]byte(test.body), test.headers, request); got != test.portable {
				t.Fatalf("portable %t, want %t", got, test.portable)
			}
		})
	}
}
