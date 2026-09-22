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
	source := strings.Replace(continuationInput, "ROUTE", slug, 1)
	headers := map[string]string{"Content-Type": "application/json", "X-OLP-Continuation": continuationClientVersion, "X-OLP-Submission-ID": resources.SubmissionID(time.Now(), uuid.New())}
	status, raw, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 200 {
		t.Fatalf("first: %d %s", status, raw)
	}
	var handle string
	var tools []any
	var text strings.Builder
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
