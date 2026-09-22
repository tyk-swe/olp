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
)

func TestContinuationHandleRechecksHistoricalNetworkCredentialRevocation(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	identity := newProfileNetworkFixture(t, true)
	stream, err := os.ReadFile("../fixtures/fidelity/v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int64
	provider := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) != 1 || len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != "profile fixture client" {
			http.Error(w, "client TLS identity required", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Api-Key") != vendorSecret || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			http.Error(w, "native API identity changed", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request map[string]json.RawMessage
		_ = json.Unmarshal(body, &request)
		if !bytes.Contains(body, []byte("Weather and time in Paris?")) {
			parityGeneration(w, "anthropic", string(request["stream"]) == "true")
			return
		}
		accepted.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream)
	}))
	provider.TLS = identity.server.TLS.Clone()
	provider.StartTLS()
	t.Cleanup(provider.Close)
	options := map[string]any{
		"network":            map[string]any{"trust_roots_pem": identity.roots, "connect_timeout_ms": 1500, "tls_handshake_timeout_ms": 1500},
		"bindings":           map[string]any{vendorModel: map[string]any{"model": "fixture-model"}},
		"operation_defaults": map[string]any{"generation": map[string]any{"dialect": "anthropic-messages", "values": map[string]any{"max_tokens": 2048, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}}},
	}
	config := map[string]any{"kind": "anthropic", "profile_id": "anthropic-messages", "profile_revision": "1", "endpoint": provider.URL + "/v1", "auth_mode": "api_key", "options": options}
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "continuation network", "configuration": config, "model": vendorModel, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	path := "/api/v3/providers/" + created["id"].(string)
	stored := h.want(owner, "POST", path+"/network-credentials", map[string]any{"credential": identity.credential}, withMatch(created, idem(uuid.NewString())), 201)
	networkID := stored["credential_id"].(string)
	options["network"].(map[string]any)["credential_id"] = networkID
	updated := h.want(owner, "PATCH", path, map[string]any{"name": "continuation network", "configuration": config}, etagHeader(stored), 200)
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)["items"].([]any)
	modelID := models[0].(map[string]any)["id"].(string)
	capabilities := []any{}
	for _, surface := range []string{"openai", "anthropic"} {
		for _, mode := range []string{"unary", "streaming"} {
			capabilities = append(capabilities, map[string]any{"operation": "generation", "surface": surface, "mode": mode})
		}
	}
	updated = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(updated), 200)
	h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(updated), 200)
	updated = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(updated, idem(uuid.NewString())), 200)
	slug := "strict-network-" + uuid.NewString()
	draft := fidelityDraft(slug, created["id"].(string))
	draft["fidelity"] = map[string]any{"mode": "strict"}
	createdRoute := h.want(owner, "POST", "/api/v3/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+createdRoute["id"].(string)+"/activate", nil, withMatch(createdRoute, idem(uuid.NewString())), 200)
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	source := strings.Replace(continuationInput, "ROUTE", slug, 1)
	headers := continuationHeaders()
	status, first, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 200 || accepted.Load() != 1 {
		t.Fatalf("mTLS continuation first turn: %d accepted=%d %s", status, accepted.Load(), first)
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
		t.Fatal("missing mTLS continuation handle")
	}
	status, recovered, _ := h.gatewayRaw("GET", "/v1/continuations/"+handle, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 200 {
		t.Fatalf("live network credential did not recover: %d %s", status, recovered)
	}
	var ready struct {
		Assistant json.RawMessage `json:"assistant"`
	}
	if json.Unmarshal(recovered, &ready) != nil || len(ready.Assistant) == 0 {
		t.Fatal("missing recoverable assistant")
	}
	updated = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/network-credentials/"+networkID+"/revoke", nil, withMatch(updated, idem(uuid.NewString())), 200)
	h.refresh()
	status, refused, _ := h.gatewayRaw("GET", "/v1/continuations/"+handle, key, nil, map[string]string{"X-OLP-Continuation": continuationClientVersion})
	if status != 409 || !bytes.Contains(refused, []byte("provider_resource_credential_unavailable")) || accepted.Load() != 1 {
		t.Fatalf("revoked network credential recovered handle: %d accepted=%d %s", status, accepted.Load(), refused)
	}
	status, refused, _ = h.gatewayRaw("POST", "/v1/chat/completions", key, strings.NewReader(source), headers)
	if status != 409 || accepted.Load() != 1 {
		t.Fatalf("revoked network credential replayed inference: %d accepted=%d %s", status, accepted.Load(), refused)
	}
	var next map[string]json.RawMessage
	_ = json.Unmarshal([]byte(source), &next)
	delete(next, "stream")
	next["messages"], _ = json.Marshal([]json.RawMessage{
		json.RawMessage(`{"role":"user","content":"Weather and time in Paris?"}`), ready.Assistant,
		json.RawMessage(`{"role":"tool","tool_call_id":"call-weather","content":"sunny"}`),
		json.RawMessage(`{"role":"tool","tool_call_id":"call-clock","content":"14:00"}`),
	})
	nextBody, _ := json.Marshal(next)
	nextHeaders := continuationHeaders()
	nextHeaders["X-OLP-Continuation-Handle"] = handle
	status, refused, _ = h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(nextBody), nextHeaders)
	if status != 409 || !bytes.Contains(refused, []byte("provider_resource_credential_unavailable")) || accepted.Load() != 1 {
		t.Fatalf("revoked network credential allowed a child Attempt: %d accepted=%d %s", status, accepted.Load(), refused)
	}
}
