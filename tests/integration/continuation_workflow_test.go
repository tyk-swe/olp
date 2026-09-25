//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/tests/fidelity"
)

const continuationClientVersion = "chat-anthropic-tools-v1"
const continuationInput = `{"model":"ROUTE","stream":true,"tools":[{"type":"function","function":{"name":"weather","description":"Weather in a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}},{"type":"function","function":{"name":"clock","description":"Time in a zone","parameters":{"type":"object","properties":{"zone":{"type":"string"}},"required":["zone"]}}}],"messages":[{"role":"user","content":"Weather and time in Paris?"}]}`

func TestPublicNegotiatedToolContinuationCommitReplayAndHistory(t *testing.T) {
	h := newAccessHarness(t)
	stream, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	nextGolden, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var capturedNext atomic.Value
	f := &strictProviderFixture{profile: "anthropic-messages"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var input map[string]json.RawMessage
		if err = json.Unmarshal(body, &input); err != nil {
			t.Error(err)
			return
		}
		if !bytes.Contains(body, []byte("Weather and time in Paris?")) {
			parityGeneration(w, "anthropic", string(input["stream"]) == "true")
			return
		}
		calls.Add(1)
		var messages []json.RawMessage
		_ = json.Unmarshal(input["messages"], &messages)
		if len(messages) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(stream)
			return
		}
		capturedNext.Store(bytes.Clone(body))
		if err := fidelity.Compare(nextGolden, body); err != nil {
			t.Error(err)
			http.Error(w, "unexpected continuation", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-final","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"Both tools completed."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":4}}`)
	}))
	t.Cleanup(f.Close)
	owner := h.owner()
	options := map[string]any{"bindings": map[string]any{vendorModel: map[string]any{"model": "fixture-model"}}, "operation_defaults": map[string]any{"generation": map[string]any{"dialect": "anthropic-messages", "values": map[string]any{"max_tokens": 2048, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}}}}
	slug, key := publishStrictProvider(t, h, owner, f, options, nil, "strict")
	key = stateKey(t, h, owner, slug, true)
	h.refresh()
	source := strings.Replace(continuationInput, "ROUTE", slug, 1)
	headers := map[string]string{"Content-Type": "application/json", "X-OLP-Continuation": continuationClientVersion, "X-OLP-Submission-ID": resources.SubmissionID(time.Now(), uuid.New())}
	status, raw, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 200 {
		t.Fatalf("first: %d %s", status, raw)
	}
	var handle string
	var tools []any
	var text strings.Builder
	var terminal json.RawMessage
	var actions json.RawMessage
	sawTool := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		if ext, ok := chunk["olp"].(map[string]any); ok {
			if value, ok := ext["handle"].(string); ok {
				handle = value
			}
			if record, ok := ext["native_terminal"]; ok {
				terminal, _ = json.Marshal(record)
			}
			if claim, ok := ext["actions"]; ok {
				actions, _ = json.Marshal(claim)
			}
		}
		for _, choice := range chunk["choices"].([]any) {
			delta := choice.(map[string]any)["delta"].(map[string]any)
			if content, ok := delta["content"].(string); ok {
				text.WriteString(content)
			}
			if items, ok := delta["tool_calls"].([]any); ok {
				sawTool = true
				// At the first ordinary actionable tool chunk, another DB reader must
				// already see both ready metadata and the authenticated complete payload.
				var ready bool
				if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp_go.provider_resources r JOIN olp_go.secrets s ON s.id=r.id WHERE r.submission_id=$1 AND r.state='ready' AND s.purpose='provider_continuation')`, headers["X-OLP-Submission-ID"]).Scan(&ready); err != nil || !ready {
					t.Fatalf("tool preceded durable dependency: ready=%t err=%v", ready, err)
				}
				for _, item := range items {
					call := item.(map[string]any)
					delete(call, "index")
					tools = append(tools, call)
				}
			}
		}
	}
	if !sawTool || len(tools) != 2 || text.String() != "beforeafter" || !strings.HasPrefix(handle, "continuation_") {
		t.Fatalf("incomplete projection: %s", raw)
	}
	if bytes.Contains(raw, []byte("opaque-fixture-signature-do-not-log")) {
		t.Fatal("opaque signature exposed")
	}
	// The terminal frame carries the accepted native outcome verbatim beside
	// the compatible client finish reason, not a narrowed reconstruction.
	if err := fidelity.Compare([]byte(`{"stop_reason":"tool_use","stop_sequence":null,"finish_reason":"tool_calls"}`), terminal); err != nil {
		t.Fatalf("native terminal record %s: %v", terminal, err)
	}
	// The terminal frame also carries the explicit actionability claim —
	// ordered call identities, independent of the recoverable handle itself.
	if err := fidelity.Compare([]byte(`{"tool_calls":["call-weather","call-clock"]}`), actions); err != nil {
		t.Fatalf("committed action claim %s: %v", actions, err)
	}
	// The read-only recovery surface reports the same committed outcome
	// records alongside the recorded delivery.
	status, recovery, _ := h.gatewayRaw("GET", "/v1/continuation-submissions/"+headers["X-OLP-Submission-ID"], key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 200 {
		t.Fatalf("submission recovery: %d %s", status, recovery)
	}
	var recovered map[string]json.RawMessage
	if err := json.Unmarshal(recovery, &recovered); err != nil {
		t.Fatal(err)
	}
	if err := fidelity.Compare([]byte(`{"tool_calls":["call-weather","call-clock"]}`), recovered["actions"]); err != nil {
		t.Fatalf("recovered action claim %s: %v", recovered["actions"], err)
	}
	if err := fidelity.Compare([]byte(`{"stop_reason":"tool_use","stop_sequence":null,"finish_reason":"tool_calls"}`), recovered["native_terminal"]); err != nil {
		t.Fatalf("recovered terminal record %s: %v", recovered["native_terminal"], err)
	}
	before := calls.Load()
	status, replay, replayHeaders := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 200 || !bytes.Equal(raw, replay) || calls.Load() != before || replayHeaders.Get("X-OLP-Delivery-Replay") != "true" {
		t.Fatalf("delivery replay: %d calls=%d/%d same=%t %s", status, calls.Load(), before, bytes.Equal(raw, replay), replay)
	}
	var next map[string]json.RawMessage
	_ = json.Unmarshal([]byte(source), &next)
	delete(next, "stream")
	messages := []any{map[string]any{"role": "user", "content": "Weather and time in Paris?"}, map[string]any{"role": "assistant", "content": text.String(), "tool_calls": tools}, map[string]any{"role": "tool", "tool_call_id": "call-weather", "content": "sunny"}, map[string]any{"role": "tool", "tool_call_id": "call-clock", "content": "14:00"}}
	next["messages"], _ = json.Marshal(messages)
	nextRaw, _ := json.Marshal(next)
	nextHeaders := map[string]string{"Content-Type": "application/json", "X-OLP-Continuation": continuationClientVersion, "X-OLP-Submission-ID": resources.SubmissionID(time.Now(), uuid.New()), "X-OLP-Continuation-Handle": handle}
	status, final, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(nextRaw), nextHeaders)
	if status != 200 || !bytes.Contains(final, []byte("Both tools completed.")) || calls.Load() != 2 {
		t.Fatalf("next: %d calls=%d %s", status, calls.Load(), final)
	}
	// The unary surface claims the same explicit actionability: a completed
	// turn commits an empty claim rather than an absent or implied one.
	var finalBody struct {
		Extension struct {
			Actions json.RawMessage `json:"actions"`
		} `json:"olp"`
	}
	if err := json.Unmarshal(final, &finalBody); err != nil {
		t.Fatal(err)
	}
	if err := fidelity.Compare([]byte(`{"tool_calls":[]}`), finalBody.Extension.Actions); err != nil {
		t.Fatalf("final action claim %s: %v", finalBody.Extension.Actions, err)
	}
	if capturedNext.Load() == nil {
		t.Fatal("native next request not observed")
	}
	// A new identity is not permission to edit the handle's historical controls.
	edited := bytes.Replace(nextRaw, []byte("Weather and time in Paris?"), []byte("edited history"), 1)
	nextHeaders["X-OLP-Submission-ID"] = resources.SubmissionID(time.Now(), uuid.New())
	status, rejected, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(edited), nextHeaders)
	if status != 400 || calls.Load() != 2 || !bytes.Contains(rejected, []byte("state_carrier")) {
		t.Fatalf("edited history: %d %s", status, rejected)
	}
}

// The scripted second turn yields one tool call with empty visible text and an
// exact fractional argument; the third completes with ordinary text. Unary
// and streaming stages carry identical native outcomes.
const matrixSecondToolSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg-matrix-2","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":40,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call-clock-2","name":"clock","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"zone\":\"UTC\",\"precise\":0.1}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":12}}

event: message_stop
data: {"type":"message_stop"}

`

const matrixFinalSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg-matrix-3","type":"message","role":"assistant","model":"fixture-model","content":[],"usage":{"input_tokens":60,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"All tools completed."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":6}}

event: message_stop
data: {"type":"message_stop"}

`

const matrixFirstUnary = `{"id":"msg-matrix-1","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"tool_use","id":"call-weather","name":"weather","input":{"city":"Paris"}},{"type":"tool_use","id":"call-clock","name":"clock","input":{"zone":"Europe/Paris","precise":0.1}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":18,"output_tokens":28}}`
const matrixSecondUnary = `{"id":"msg-matrix-2","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"tool_use","id":"call-clock-2","name":"clock","input":{"zone":"UTC","precise":0.1}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":40,"output_tokens":12}}`
const matrixFinalUnary = `{"id":"msg-matrix-3","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"All tools completed."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":60,"output_tokens":6}}`

// matrixTurn folds one raw committed delivery — streamed SSE bytes or a unary
// body — into the outcome pieces the negotiated carrier exposes: the ready
// handle, the canonical assistant, the explicit ordered action claim and the
// finish reason. Both surfaces must produce the same answers.
func matrixTurn(t *testing.T, raw []byte, streamed bool) (string, map[string]any, []string, string) {
	t.Helper()
	if !streamed {
		var body struct {
			Choices []struct {
				Message      map[string]any `json:"message"`
				FinishReason string         `json:"finish_reason"`
			} `json:"choices"`
			Extension struct {
				Ready   bool   `json:"ready"`
				Handle  string `json:"handle"`
				Actions struct {
					ToolCalls []string `json:"tool_calls"`
				} `json:"actions"`
			} `json:"olp"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || len(body.Choices) != 1 {
			t.Fatalf("unary delivery: %v %s", err, raw)
		}
		if !body.Extension.Ready || !strings.HasPrefix(body.Extension.Handle, "continuation_") {
			t.Fatalf("unary delivery not ready: %s", raw)
		}
		return body.Extension.Handle, body.Choices[0].Message, body.Extension.Actions.ToolCalls, body.Choices[0].FinishReason
	}
	var handle, finish string
	var claim []string
	text := ""
	calls := map[int]map[string]any{}
	ready := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		ext, _ := chunk["olp"].(map[string]any)
		choices, _ := chunk["choices"].([]any)
		for _, choice := range choices {
			selected := choice.(map[string]any)
			if delta, ok := selected["delta"].(map[string]any); ok {
				if content, ok := delta["content"].(string); ok {
					text += content
				}
				items, _ := delta["tool_calls"].([]any)
				for _, item := range items {
					call := item.(map[string]any)
					index := int(call["index"].(float64))
					entry := calls[index]
					if entry == nil {
						entry = map[string]any{"id": "", "type": "function", "function": map[string]any{"name": "", "arguments": ""}}
						calls[index] = entry
					}
					if id, ok := call["id"].(string); ok {
						entry["id"] = entry["id"].(string) + id
					}
					if fn, ok := call["function"].(map[string]any); ok {
						if name, ok := fn["name"].(string); ok {
							entry["function"].(map[string]any)["name"] = entry["function"].(map[string]any)["name"].(string) + name
						}
						if args, ok := fn["arguments"].(string); ok {
							entry["function"].(map[string]any)["arguments"] = entry["function"].(map[string]any)["arguments"].(string) + args
						}
					}
				}
			}
			if reason, ok := selected["finish_reason"].(string); ok && reason != "" {
				finish = reason
			}
		}
		if ext["ready"] == true {
			ready = true
			handle, _ = ext["handle"].(string)
			if action, ok := ext["actions"].(map[string]any); ok {
				ids, _ := action["tool_calls"].([]any)
				for _, id := range ids {
					claim = append(claim, id.(string))
				}
			}
		}
	}
	if !ready || !strings.HasPrefix(handle, "continuation_") {
		t.Fatalf("stream delivery not ready: %s", raw)
	}
	ordered := []any{}
	for index := 0; index < len(calls); index++ {
		ordered = append(ordered, calls[index])
	}
	assistant := map[string]any{"role": "assistant", "content": text}
	if len(ordered) > 0 {
		assistant["tool_calls"] = ordered
	}
	return handle, assistant, claim, finish
}

// matrixNext builds the next request: the accumulated message history, the
// exact assistant representation and one ordered result per claimed call. The
// mode controls only how the next delivery arrives. It returns the extended
// history the following turn must prefix.
func matrixNext(t *testing.T, source map[string]json.RawMessage, streamed bool, handle, submission string, history []any, assistant map[string]any, claim []string, results []string) (map[string]string, []byte, []any) {
	t.Helper()
	if len(claim) != len(results) {
		t.Fatalf("results do not match the committed claim: %v vs %v", claim, results)
	}
	next := append(append([]any{}, history...), assistant)
	for index, id := range claim {
		next = append(next, map[string]any{"role": "tool", "tool_call_id": id, "content": results[index]})
	}
	body := map[string]any{"model": source["model"], "tools": source["tools"], "messages": next}
	if streamed {
		body["stream"] = true
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Content-Type": "application/json", "X-OLP-Continuation": continuationClientVersion, "X-OLP-Submission-ID": submission, "X-OLP-Continuation-Handle": handle}
	return headers, raw, next
}

// Every client-visible transition — unary→unary, unary→stream, stream→unary
// and stream→stream — crosses three turns over two committed tool yields and
// a final turn, each exposing the same completed-turn outcome contract. The
// recovered transition re-reads the committed first turn without dispatching.
func TestPublicNegotiatedContinuationTransitionMatrix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		first   bool // streamed first turn
		second  bool // streamed second turn
		third   bool // streamed final turn
		recover bool // replay the first committed turn through recovery
	}{
		{"stream then stream then unary", true, true, false, true},
		{"stream then unary then stream", true, false, true, false},
		{"unary then stream then unary", false, true, false, true},
		{"unary then unary then stream", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAccessHarness(t)
			streamFixture, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			var lastBody atomic.Value
			slug, key := continuationBarrierFixture(t, h, func(w http.ResponseWriter, _ *http.Request, body []byte) {
				calls.Add(1)
				lastBody.Store(bytes.Clone(body))
				var input map[string]json.RawMessage
				if err := json.Unmarshal(body, &input); err != nil {
					t.Error(err)
					return
				}
				var messages []json.RawMessage
				_ = json.Unmarshal(input["messages"], &messages)
				var sse, unary string
				switch len(messages) {
				case 1:
					sse, unary = string(streamFixture), matrixFirstUnary
				case 3:
					sse, unary = matrixSecondToolSSE, matrixSecondUnary
				default:
					sse, unary = matrixFinalSSE, matrixFinalUnary
				}
				if string(input["stream"]) == "true" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, sse)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, unary)
			})
			source := map[string]json.RawMessage{}
			if err := json.Unmarshal([]byte(strings.Replace(continuationInput, "ROUTE", slug, 1)), &source); err != nil {
				t.Fatal(err)
			}
			var history []any
			if err := json.Unmarshal(source["messages"], &history); err != nil {
				t.Fatal(err)
			}
			if !tc.first {
				delete(source, "stream")
			}
			firstRaw, _ := json.Marshal(source)
			headers := continuationHeaders()
			status, raw, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(firstRaw), headers)
			if status != 200 {
				t.Fatalf("first turn: %d %s", status, raw)
			}
			handle, assistant, claim, finish := matrixTurn(t, raw, tc.first)
			if finish != "tool_calls" || len(claim) != 2 || claim[0] != "call-weather" || claim[1] != "call-clock" {
				t.Fatalf("first turn actions: finish=%s claim=%v", finish, claim)
			}
			var firstCalls []string
			for _, call := range assistant["tool_calls"].([]any) {
				firstCalls = append(firstCalls, call.(map[string]any)["id"].(string))
			}
			// The explicit claim names exactly the assistant's ordered calls.
			if len(firstCalls) != len(claim) {
				t.Fatalf("claim does not correspond to assistant calls: %v vs %v", claim, firstCalls)
			}
			for index := range claim {
				if claim[index] != firstCalls[index] {
					t.Fatalf("claim reordered assistant calls: %v vs %v", claim, firstCalls)
				}
			}
			dispatched := calls.Load()
			if tc.recover {
				// Recovery returns the same committed outcome — including the
				// explicit claim — without dispatching another inference.
				status, recovery, _ := h.gatewayRaw("GET", "/v1/continuation-submissions/"+headers["X-OLP-Submission-ID"], key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
				if status != 200 {
					t.Fatalf("recovery: %d %s", status, recovery)
				}
				var state map[string]json.RawMessage
				if err := json.Unmarshal(recovery, &state); err != nil {
					t.Fatal(err)
				}
				expectedClaim, _ := json.Marshal(map[string]any{"tool_calls": claim})
				if err := fidelity.Compare(expectedClaim, state["actions"]); err != nil {
					t.Fatalf("recovered claim %s: %v", state["actions"], err)
				}
				if calls.Load() != dispatched {
					t.Fatal("recovery dispatched another inference")
				}
			}
			// Out-of-order or duplicated results are rejected before dispatch.
			for _, bad := range [][]any{
				{map[string]any{"role": "assistant", "content": assistant["content"], "tool_calls": assistant["tool_calls"]}, map[string]any{"role": "tool", "tool_call_id": claim[1], "content": "14:00"}, map[string]any{"role": "tool", "tool_call_id": claim[0], "content": "sunny"}},
				{map[string]any{"role": "assistant", "content": assistant["content"], "tool_calls": assistant["tool_calls"]}, map[string]any{"role": "tool", "tool_call_id": claim[0], "content": "sunny"}, map[string]any{"role": "tool", "tool_call_id": claim[0], "content": "sunny"}},
			} {
				badMessages := append(append([]any{}, history...), bad...)
				badRaw, _ := json.Marshal(map[string]any{"model": source["model"], "tools": source["tools"], "messages": badMessages})
				badHeaders := continuationHeaders()
				badHeaders["X-OLP-Continuation-Handle"] = handle
				status, rejected, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(badRaw), badHeaders)
				if status != 400 || calls.Load() != dispatched || !bytes.Contains(rejected, []byte("state_carrier")) {
					t.Fatalf("uncorresponding results dispatched: %d calls=%d %s", status, calls.Load(), rejected)
				}
			}
			var next []any
			secondHeaders, secondRaw, next := matrixNext(t, source, tc.second, handle, resources.SubmissionID(time.Now(), uuid.New()), history, assistant, claim, []string{"sunny", "14:00"})
			status, raw, _ = h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(secondRaw), secondHeaders)
			if status != 200 {
				t.Fatalf("second turn: %d %s", status, raw)
			}
			history = next
			handle, assistant, claim, finish = matrixTurn(t, raw, tc.second)
			if finish != "tool_calls" || len(claim) != 1 || claim[0] != "call-clock-2" {
				t.Fatalf("second turn actions: finish=%s claim=%v", finish, claim)
			}
			// The empty-text second yield keeps an exact canonical assistant;
			// the fractional argument string survives byte-for-byte.
			if assistant["content"] != "" {
				t.Fatalf("second turn text: %q", assistant["content"])
			}
			secondCalls := assistant["tool_calls"].([]any)
			if got := secondCalls[0].(map[string]any)["function"].(map[string]any)["arguments"].(string); got != `{"zone":"UTC","precise":0.1}` {
				t.Fatalf("argument string changed: %s", got)
			}
			thirdHeaders, thirdRaw, next := matrixNext(t, source, tc.third, handle, resources.SubmissionID(time.Now(), uuid.New()), history, assistant, claim, []string{"noon"})
			history = next
			status, raw, _ = h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(thirdRaw), thirdHeaders)
			if status != 200 {
				t.Fatalf("third turn: %d %s", status, raw)
			}
			handle, assistant, claim, finish = matrixTurn(t, raw, tc.third)
			if finish != "stop" || len(claim) != 0 || assistant["content"] != "All tools completed." {
				t.Fatalf("final turn actions: finish=%s claim=%v", finish, claim)
			}
			if calls.Load() != 3 {
				t.Fatalf("provider dispatches=%d", calls.Load())
			}
			// The third turn's unary surface claims the explicit empty set.
			if lastBody.Load() == nil || !bytes.Contains(lastBody.Load().([]byte), []byte(`"precise":0.1`)) {
				t.Fatalf("fractional argument lexeme changed: %s", lastBody.Load())
			}
		})
	}
}
