//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestContinuationRequiresCurrentProviderStatePolicy(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	stream, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	slug, _ := continuationBarrierFixtureOwner(t, h, owner, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream)
	})
	keyRecord := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "continuation policy owner", "scopes": []string{"inference"},
		"allowed_routes": []string{slug}, "allow_provider_state": true,
	}, idem(uuid.NewString()), 201)
	key := keyRecord["secret"].(string)
	keyPath := "/api/v1/api-keys/" + keyRecord["id"].(string)
	deniedKey := stateKey(t, h, owner, slug, false)
	h.refresh()
	source := strings.Replace(continuationInput, "ROUTE", slug, 1)
	headers := continuationHeaders()
	status, denied, _ := h.gatewayRaw("POST", "/v1/chat/completions", deniedKey, strings.NewReader(source), headers)
	if status != 400 || !bytes.Contains(denied, []byte("policy_conflict")) || calls.Load() != 0 {
		t.Fatalf("state-disabled key dispatched: status=%d calls=%d %s", status, calls.Load(), denied)
	}
	status, first, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 200 || calls.Load() != 1 {
		t.Fatalf("state-enabled key: status=%d calls=%d %s", status, calls.Load(), first)
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
	if handle == "" {
		t.Fatal("committed response omitted continuation handle")
	}
	for _, path := range []string{"/v1/continuations/" + handle, "/v1/continuation-submissions/" + headers["X-OLP-Submission-ID"]} {
		status, denied, _ = h.gatewayRaw("GET", path, deniedKey, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
		if status != 400 || !bytes.Contains(denied, []byte("policy_conflict")) {
			t.Fatalf("state-disabled recovery %s: %d %s", path, status, denied)
		}
	}
	keyRecord = h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "PATCH", keyPath, map[string]any{"allow_provider_state": false}, etagHeader(keyRecord), 200)
	h.refresh()
	status, denied, _ = h.gatewayRaw("GET", "/v1/continuations/"+handle, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 400 || !bytes.Contains(denied, []byte("policy_conflict")) {
		t.Fatalf("policy revoked after commit: %d %s", status, denied)
	}
	status, denied, _ = h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 400 || !bytes.Contains(denied, []byte("policy_conflict")) || calls.Load() != 1 {
		t.Fatalf("policy-revoked replay dispatched: %d calls=%d %s", status, calls.Load(), denied)
	}
	keyRecord = h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "PATCH", keyPath, map[string]any{"allow_provider_state": true}, etagHeader(keyRecord), 200)
	h.refresh()
	status, ready, _ := h.gatewayRaw("GET", "/v1/continuations/"+handle, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 200 || !bytes.Contains(ready, []byte(`"state":"ready"`)) || calls.Load() != 1 {
		t.Fatalf("restored policy did not recover same work: %d calls=%d %s", status, calls.Load(), ready)
	}
}
