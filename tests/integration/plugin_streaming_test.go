//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/internal/usage"
)

const streamedAnswer = "Hello from the streaming-only upstream"

// streamingUpstream is the fictional upstream the reference plugin's
// reference-streaming profile places requests at: OpenAI Responses under
// /streaming/v1 that serves only streaming requests. It refuses, and counts,
// any request that does not ask for a stream, and it delivers its output item
// only in response.output_item.done, leaving the terminal response's output
// empty.
type streamingUpstream struct {
	*httptest.Server
	refused atomic.Int64
}

func newStreamingUpstream(t *testing.T) *streamingUpstream {
	t.Helper()
	u := &streamingUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token "+pluginCredential || r.Header.Get("X-Reference-Client") != "olp" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "Unknown token", "type": "invalid_request_error", "code": "invalid_api_key"}})
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/streaming/v1/responses" || json.NewDecoder(r.Body).Decode(&body) != nil || !body.Stream {
			u.refused.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "Stream must be set to true", "type": "invalid_request_error"}})
			return
		}
		response := func(status string) map[string]any {
			return map[string]any{"id": "resp_streaming", "object": "response", "created_at": 1, "status": status, "model": body.Model, "output": []any{}}
		}
		completed := response("completed")
		completed["usage"] = map[string]any{"input_tokens": 5, "output_tokens": 7, "total_tokens": 12}
		message := func(status string, content ...any) map[string]any {
			return map[string]any{"id": "msg_streaming", "type": "message", "role": "assistant", "status": status, "content": content}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, event := range []map[string]any{
			{"type": "response.created", "response": response("in_progress")},
			{"type": "response.output_item.added", "output_index": 0, "item": message("in_progress")},
			{"type": "response.output_text.delta", "item_id": "msg_streaming", "output_index": 0, "content_index": 0, "delta": streamedAnswer},
			{"type": "response.output_item.done", "output_index": 0, "item": message("completed", map[string]any{"type": "output_text", "text": streamedAnswer, "annotations": []any{}})},
			{"type": "response.completed", "response": completed},
		} {
			event["sequence_number"] = i
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// A plugin profile whose upstream serves only streaming requests serves
// non-streaming callers, the official OpenAI SDK among them: OLP streams every
// request and aggregates the stream into the Responses result, which it
// accounts and prices as the equivalent streaming request.
func TestPluginProfileForcingStreamingServesNonStreamingCallers(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tests/sdk-smoke/node_modules/openai")); err != nil {
		t.Fatal("pinned OpenAI JavaScript SDK is missing; run pnpm install --frozen-lockfile")
	}
	h := newAccessHarness(t)
	sink := &captureSink{}
	emitter := usage.NewEmitter(16)
	h.Gateway.Sink = &gateway.AccountingSink{Emitter: emitter, Next: sink}
	owner := h.owner()
	upstream := newStreamingUpstream(t)
	module := testutil.BuildPlugin(t, "./sdk/plugin/reference", "-X=main.upstream="+upstream.URL+"/v1")
	digest := digestOf(module)
	installed := h.want(owner, "POST", "/api/v1/plugins", module, wasm, 201)
	for _, profile := range installed["manifest"].(map[string]any)["profiles"].([]any) {
		if p := profile.(map[string]any); p["id"] == "reference-streaming" && p["hosting"].(map[string]any)["force_streaming"] != true {
			t.Fatalf("the owner reviews %v", p)
		}
	}
	approvePlugin(t, h, owner, installed)
	for _, profile := range h.want(owner, "GET", "/api/v1/provider-profiles", nil, nil, 200)["items"].([]any) {
		if p := profile.(map[string]any); p["id"] == "reference-streaming" && p["strict"] != false {
			t.Fatalf("catalogued %v", p)
		}
	}

	create := map[string]any{"name": "Streaming only", "credential": pluginCredential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-streaming", "profile_revision": digest}}
	created := h.want(owner, "POST", "/api/v1/providers", create, idem(uuid.NewString()), 201)
	// The upstream refuses anything but a stream, so certifying the unary
	// capability proves that certification aggregates the stream too.
	certifyPluginProvider(t, h, owner, "/api/v1/providers/"+created["id"].(string))
	key := publishRoute(t, h, owner, transformed(fidelityDraft("streaming-only", created["id"])), "Streaming only")
	price := map[string]any{"provider_kind": "plugin", "provider_id": created["id"], "model": vendorModel, "operation": "generation", "currency": "USD", "input_per_million": "2", "output_per_million": "4"}
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "prices": []any{price}}, idem(uuid.NewString()), 201)
	h.refresh()

	status, reply, _ := h.gateway("POST", "/v1/responses", key, map[string]any{"model": "streaming-only", "input": "hi"})
	usage, _ := reply["usage"].(map[string]any)
	if status != 200 || reply["object"] != "response" || reply["model"] != "streaming-only" || reply["status"] != "completed" ||
		!strings.Contains(fmt.Sprint(reply["output"]), streamedAnswer) || usage["total_tokens"] != float64(12) {
		t.Fatalf("non-streaming Responses through a streaming-only upstream: %d %v", status, reply)
	}
	unary := sink.last()

	cmd := exec.CommandContext(t.Context(), "node", "tests/sdk-smoke/forced-streaming.mjs")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "OLP_RESPONSES_BASE="+h.HTTP.URL, "OLP_RESPONSES_ROUTE=streaming-only", "OLP_RESPONSES_KEY="+key, "OLP_RESPONSES_EXPECT="+streamedAnswer)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("non-streaming official OpenAI SDK call: %v\n%s", err, output)
	}

	streamed, _ := json.Marshal(map[string]any{"model": "streaming-only", "input": "hi", "stream": true})
	code, events, _ := h.gatewayRaw("POST", "/v1/responses", key, bytes.NewReader(streamed), map[string]string{"Content-Type": "application/json"})
	if code != 200 || !bytes.Contains(events, []byte(`"delta":"`+streamedAnswer+`"`)) || !bytes.Contains(events, []byte("event: response.completed")) {
		t.Fatalf("streaming Responses through a streaming-only upstream: %d %s", code, events)
	}
	stream := sink.last()
	if refused := upstream.refused.Load(); refused != 0 {
		t.Fatalf("OLP sent the upstream %d requests that did not ask for a stream", refused)
	}

	// The aggregated request and the streaming one leave the same usage and
	// pricing records.
	emitter.Close()
	flush, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	emitter.RunWriter(flush, persistedUsageStream{h.Pool}, "fixture", slog.Default())
	cancel()
	type record struct {
		Charge             string
		Input, Output      int64
		Cost               string
		Unpriced, Observed bool
	}
	recorded := func(event gateway.Envelope) (r record) {
		if err := h.Pool.QueryRow(t.Context(), `SELECT charge_status, input_tokens, output_tokens, estimated_cost::text, unpriced, usage_observed
			FROM olp.attempt_usage_facts WHERE request_id = $1::uuid AND attempt_ordinal = 1`, event.AccountingID).
			Scan(&r.Charge, &r.Input, &r.Output, &r.Cost, &r.Unpriced, &r.Observed); err != nil {
			t.Fatalf("load the usage of request %s: %v", event.AccountingID, err)
		}
		return r
	}
	want := record{Charge: "billable", Input: 5, Output: 7, Cost: "0.000038000000", Observed: true}
	if aggregated, streaming := recorded(unary), recorded(stream); unary.Mode != "unary" || stream.Mode != "streaming" || aggregated != want || streaming != want {
		t.Fatalf("recorded %+v for the aggregated request and %+v for the streaming one, want %+v", aggregated, streaming, want)
	}
}
