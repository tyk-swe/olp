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

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/tests/fidelity"
)

func TestActiveContinuationKeepsCompatibleHistoricalRevisionAcrossRestart(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	events, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	nextGolden, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var historicalCalls, replacementCalls atomic.Int64
	original := &strictProviderFixture{profile: "anthropic-messages"}
	original.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("Weather and time in Paris?")) {
			var probe map[string]json.RawMessage
			_ = json.Unmarshal(body, &probe)
			parityGeneration(w, "anthropic", string(probe["stream"]) == "true")
			return
		}
		if r.Header.Get("X-Api-Key") != vendorSecret || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			http.Error(w, "historical API credential or revision changed", 400)
			return
		}
		historicalCalls.Add(1)
		var input map[string]json.RawMessage
		_ = json.Unmarshal(body, &input)
		var messages []json.RawMessage
		_ = json.Unmarshal(input["messages"], &messages)
		if len(messages) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(events)
			return
		}
		if err := fidelity.Compare(nextGolden, body); err != nil {
			http.Error(w, "historical continuation differs: "+err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-revision-final","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"Both tools completed."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":4}}`)
	}))
	t.Cleanup(original.Close)
	replacement := &strictProviderFixture{profile: "anthropic-messages"}
	replacement.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		replacementCalls.Add(1)
		var probe map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&probe)
		parityGeneration(w, "anthropic", string(probe["stream"]) == "true")
	}))
	t.Cleanup(replacement.Close)
	options := map[string]any{"bindings": map[string]any{vendorModel: map[string]any{"model": "fixture-model"}}, "operation_defaults": map[string]any{"generation": map[string]any{"dialect": "anthropic-messages", "values": map[string]any{"max_tokens": 2048, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}}}}
	slug, _ := publishStrictProvider(t, h, owner, original, options, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	source := strings.Replace(continuationInput, "ROUTE", slug, 1)
	headers := continuationHeaders()
	status, first, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 200 || historicalCalls.Load() != 1 {
		t.Fatalf("initial continuation: %d old=%d %s", status, historicalCalls.Load(), first)
	}
	var handle string
	for line := range strings.SplitSeq(string(first), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var frame struct {
			OLP struct {
				Handle string `json:"handle"`
			} `json:"olp"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.OLP.Handle != "" {
			handle = frame.OLP.Handle
		}
	}
	if handle == "" {
		t.Fatal("missing durable handle")
	}
	providerPath := "/api/v1/providers/" + original.providerID
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	slots := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)
	historicalCredential := slots["items"].([]any)[0].(map[string]any)["credential_version_id"].(string)
	h.want(owner, "POST", providerPath+"/credentials", map[string]any{"credential": "rotated-fixture-secret"}, withMatch(detail, idem(uuid.NewString())), 201)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	configuration := detail["configuration"].(map[string]any)
	configuration["endpoint"] = replacement.URL + "/v1"
	configuration["options"].(map[string]any)["operation_defaults"].(map[string]any)["generation"].(map[string]any)["values"].(map[string]any)["max_tokens"] = 999
	detail = h.want(owner, "PATCH", providerPath, map[string]any{"name": "Compatible replacement", "configuration": configuration}, etagHeader(detail), 200)
	modelID := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 200)
	h.refresh()
	probeCalls := replacementCalls.Load()
	h = newAccessHarnessOn(t, h.Pool, h.DBURL)
	h.refresh()
	status, recovered, _ := h.gatewayRaw("GET", "/v1/continuations/"+handle, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 200 {
		t.Fatalf("historical handle after new release: %d %s", status, recovered)
	}
	var ready struct {
		Assistant json.RawMessage `json:"assistant"`
	}
	if err := json.Unmarshal(recovered, &ready); err != nil || len(ready.Assistant) == 0 {
		t.Fatalf("invalid recovered assistant: %v %s", err, recovered)
	}
	var next map[string]json.RawMessage
	_ = json.Unmarshal([]byte(source), &next)
	delete(next, "stream")
	next["messages"], _ = json.Marshal([]json.RawMessage{json.RawMessage(`{"role":"user","content":"Weather and time in Paris?"}`), ready.Assistant, json.RawMessage(`{"role":"tool","tool_call_id":"call-weather","content":"sunny"}`), json.RawMessage(`{"role":"tool","tool_call_id":"call-clock","content":"14:00"}`)})
	nextBody, _ := json.Marshal(next)
	nextHeaders := continuationHeaders()
	nextHeaders["X-OLP-Continuation-Handle"] = handle
	status, final, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(nextBody), nextHeaders)
	if status != 200 || !bytes.Contains(final, []byte("Both tools completed.")) || historicalCalls.Load() != 2 || replacementCalls.Load() != probeCalls {
		t.Fatalf("historical route changed: %d old=%d new=%d probe=%d %s", status, historicalCalls.Load(), replacementCalls.Load(), probeCalls, final)
	}
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/credentials/"+historicalCredential+"/revoke", nil, withMatch(detail, idem(uuid.NewString())), 200)
	h.refresh()
	status, refused, _ := h.gatewayRaw("GET", "/v1/continuations/"+handle, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 409 || !bytes.Contains(refused, []byte("provider_resource_credential_unavailable")) || historicalCalls.Load() != 2 || replacementCalls.Load() != probeCalls {
		t.Fatalf("revoked historical credential recovered: %d old=%d new=%d probe=%d %s", status, historicalCalls.Load(), replacementCalls.Load(), probeCalls, refused)
	}
}
