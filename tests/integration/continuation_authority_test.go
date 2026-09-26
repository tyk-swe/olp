//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/tests/fidelity"
)

func TestPublicContinuationBranchesStayOwnedAndExpire(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	stream, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	nextGolden, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	slug, _ := continuationBarrierFixtureOwner(t, h, owner, func(w http.ResponseWriter, r *http.Request, body []byte) {
		calls.Add(1)
		var source map[string]json.RawMessage
		_ = json.Unmarshal(body, &source)
		var messages []json.RawMessage
		_ = json.Unmarshal(source["messages"], &messages)
		if len(messages) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(stream)
			return
		}
		expected := nextGolden
		if bytes.Contains(body, []byte(`"content":"rainy"`)) {
			expected = bytes.Replace(nextGolden, []byte(`"content": "sunny"`), []byte(`"content": "rainy"`), 1)
		}
		if err := fidelity.Compare(expected, body); err != nil {
			http.Error(w, "branch changed native dependencies: "+err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-branch-final","type":"message","role":"assistant","model":"fixture-model","content":[{"type":"text","text":"Branch completed."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":4}}`)
	})
	secondSlug := "strict-branch-" + uuid.NewString()
	providerID := h.Runtime.Release().Snapshot.Routes[slug].Targets[0].ProviderID
	draftInput := fidelityDraft(secondSlug, providerID)
	draftInput["fidelity"] = map[string]any{"mode": "strict"}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	keyRecord := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "branch owner", "scopes": []string{"inference"}, "allowed_routes": []string{slug, secondSlug}, "allow_provider_state": true}, idem(uuid.NewString()), 201)
	key := keyRecord["secret"].(string)
	otherKey := stateKey(t, h, owner, slug, true)
	h.refresh()
	source := strings.Replace(continuationInput, "ROUTE", slug, 1)
	headers := continuationHeaders()
	status, first, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 200 || calls.Load() != 1 {
		t.Fatalf("initial branch: %d calls=%d %s", status, calls.Load(), first)
	}
	var handle string
	for line := range strings.SplitSeq(string(first), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk struct {
			OLP struct {
				Handle string `json:"handle"`
			} `json:"olp"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk.OLP.Handle != "" {
			handle = chunk.OLP.Handle
		}
	}
	if !strings.HasPrefix(handle, "continuation_") {
		t.Fatal("missing parent continuation")
	}
	recoveryPath := "/v1/continuations/" + handle
	status, recovered, _ := h.gatewayRaw("GET", recoveryPath, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 200 {
		t.Fatalf("parent recovery: %d %s", status, recovered)
	}
	var ready struct {
		Assistant json.RawMessage `json:"assistant"`
	}
	if json.Unmarshal(recovered, &ready) != nil || len(ready.Assistant) == 0 {
		t.Fatal("missing owned assistant")
	}
	status, denied, _ := h.gatewayRaw("GET", recoveryPath, otherKey, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 404 || bytes.Contains(denied, []byte("Branch completed")) || calls.Load() != 1 {
		t.Fatalf("cross-key recovery: %d calls=%d %s", status, calls.Load(), denied)
	}
	tampered := handle[:len(handle)-1] + map[bool]string{true: "0", false: "1"}[handle[len(handle)-1] != '0']
	status, denied, _ = h.gatewayRaw("GET", "/v1/continuations/"+tampered, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 404 || calls.Load() != 1 {
		t.Fatalf("tampered recovery: %d calls=%d %s", status, calls.Load(), denied)
	}
	var prior map[string]json.RawMessage
	_ = json.Unmarshal([]byte(source), &prior)
	delete(prior, "stream")
	var children []string
	for _, weather := range []string{"sunny", "rainy"} {
		prior["messages"], _ = json.Marshal([]json.RawMessage{
			json.RawMessage(`{"role":"user","content":"Weather and time in Paris?"}`), ready.Assistant,
			json.RawMessage(`{"role":"tool","tool_call_id":"call-weather","content":"` + weather + `"}`),
			json.RawMessage(`{"role":"tool","tool_call_id":"call-clock","content":"14:00"}`),
		})
		next, _ := json.Marshal(prior)
		nextHeaders := continuationHeaders()
		nextHeaders["X-OLP-Continuation-Handle"] = handle
		status, final, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(next), nextHeaders)
		if status != 200 || !bytes.Contains(final, []byte("Branch completed.")) {
			t.Fatalf("independent branch %s: %d %s", weather, status, final)
		}
		var child struct {
			OLP struct {
				Handle string `json:"handle"`
			} `json:"olp"`
		}
		if json.Unmarshal(final, &child) != nil || child.OLP.Handle == "" || child.OLP.Handle == handle {
			t.Fatalf("branch did not create immutable child: %s", final)
		}
		children = append(children, child.OLP.Handle)
	}
	if len(children) != 2 || children[0] == children[1] || calls.Load() != 3 {
		t.Fatalf("branches conflated: %+v calls=%d", children, calls.Load())
	}
	status, _, _ = h.gatewayRaw("GET", recoveryPath, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 200 {
		t.Fatal("parent mutated after independent branches")
	}
	prior["model"], _ = json.Marshal(secondSlug)
	wrongRoute, _ := json.Marshal(prior)
	wrongHeaders := continuationHeaders()
	wrongHeaders["X-OLP-Continuation-Handle"] = handle
	status, denied, _ = h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(wrongRoute), wrongHeaders)
	if status != 409 || !bytes.Contains(denied, []byte("continuation_mismatch")) || calls.Load() != 3 {
		t.Fatalf("cross-route handle dispatched: %d calls=%d %s", status, calls.Load(), denied)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.provider_resources SET expires_at=now()-interval '1 second' WHERE submission_id=$1`, headers["X-OLP-Submission-ID"]); err != nil {
		t.Fatal(err)
	}
	status, denied, _ = h.gatewayRaw("GET", recoveryPath, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 404 || calls.Load() != 3 {
		t.Fatalf("expired parent recovered: %d calls=%d %s", status, calls.Load(), denied)
	}
	// A child's complete encrypted dependencies remain independent of a now
	// expired logical parent; the route/key/credential are still checked live.
	status, childState, _ := h.gatewayRaw("GET", "/v1/continuations/"+children[0], key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 200 || !bytes.Contains(childState, []byte("Branch completed.")) {
		t.Fatalf("child lost its complete branch: %d %s", status, childState)
	}
	keyPath := "/api/v1/api-keys/" + keyRecord["id"].(string)
	keyRecord = h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "POST", keyPath+"/revoke", nil, withMatch(keyRecord, idem(uuid.NewString())), 200)
	h.refresh()
	status, denied, _ = h.gatewayRaw("GET", "/v1/continuations/"+children[0], key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 401 || calls.Load() != 3 {
		t.Fatalf("revoked key recovered child: %d calls=%d %s", status, calls.Load(), denied)
	}
}
