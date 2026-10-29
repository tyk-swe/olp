//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	vendorSecret  = "vendor-secret-1"
	vendorRotated = "vendor-secret-2"
	vendorModel   = "fixture-model"
	vendorAnswer  = "hello from the fixture vendor"
	routeSlug     = "team-chat"
)

// vendor is a minimal OpenAI-compatible upstream with switchable failure and
// stream pacing so the scenario can exercise certification failure and
// in-flight publication changes.
type vendor struct {
	*httptest.Server
	mu       sync.Mutex
	secrets  map[string]bool
	fail     atomic.Bool
	truncate atomic.Bool
	delay    atomic.Int64
	chats    atomic.Int64
	models   atomic.Int64
	cancels  atomic.Int64
}

func newVendor(t *testing.T) *vendor {
	v := &vendor{secrets: map[string]bool{vendorSecret: true}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		v.models.Add(1)
		if v.fail.Load() {
			http.Error(w, `{"error":{"message":"boom","type":"server_error"}}`, http.StatusInternalServerError)
			return
		}
		if !v.authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"`+vendorModel+`","object":"model"},{"id":"other-model","object":"model"}]}`)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		v.chats.Add(1)
		if !v.authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		if v.fail.Load() {
			http.Error(w, `{"error":{"message":"boom","type":"server_error"}}`, http.StatusInternalServerError)
			return
		}
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != vendorModel {
			http.Error(w, `{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`, http.StatusNotFound)
			return
		}
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`, vendorModel, vendorAnswer)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if v.truncate.Load() {
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial \"},\"finish_reason\":null}]}\n\n", vendorModel)
			flusher.Flush()
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
				}
			}
			return
		}
		delay := time.Duration(v.delay.Load())
		for _, word := range strings.SplitAfter(vendorAnswer, " ") {
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", vendorModel, word)
			flusher.Flush()
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-r.Context().Done():
					v.cancels.Add(1)
					return
				}
			}
		}
		fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10}}\n\ndata: [DONE]\n\n", vendorModel)
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if !v.authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		var input struct {
			Model string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Model != vendorModel {
			http.Error(w, "unknown model", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.25,0.5]}],"model":%q,"usage":{"prompt_tokens":3,"total_tokens":3}}`, vendorModel)
	})
	mux.HandleFunc("POST /v1/moderations", func(w http.ResponseWriter, r *http.Request) {
		if !v.authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"mod-1","model":%q,"results":[{"flagged":true,"categories":{"harassment":true},"category_scores":{"harassment":0.9}}]}`, vendorModel)
	})
	mux.HandleFunc("POST /v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		if !v.authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"rr-1","results":[{"index":1,"relevance_score":0.9,"document":"b"},{"index":0,"relevance_score":0.4,"document":"a"}],"meta":{"billed_units":{"search_units":1.5}}}`)
	})
	mux.HandleFunc("POST /v1/responses/input_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !v.authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"response.input_tokens","input_tokens":42}`)
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if !v.authorized(r) {
			http.Error(w, `{"error":{"message":"bad key","code":"invalid_api_key"}}`, http.StatusUnauthorized)
			return
		}
		if v.fail.Load() {
			http.Error(w, `{"error":{"message":"boom","type":"server_error"}}`, http.StatusInternalServerError)
			return
		}
		var input struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Model != vendorModel {
			http.Error(w, "unknown model", http.StatusNotFound)
			return
		}
		writeResponsesFixture(w, input.Model, vendorAnswer, input.Stream)
	})
	v.Server = httptest.NewServer(mux)
	t.Cleanup(v.Close)
	return v
}

func (v *vendor) authorized(r *http.Request) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.secrets[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
}

func (v *vendor) accept(secret string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.secrets[secret] = true
}

func withMatch(record map[string]any, extra map[string]string) map[string]string {
	headers := etagHeader(record)
	for k, val := range extra {
		headers[k] = val
	}
	return headers
}

func problemCode(t *testing.T, body map[string]any) string {
	t.Helper()
	typ, _ := body["type"].(string)
	if typ == "" {
		t.Fatalf("not a problem document: %v", body)
	}
	return typ[strings.LastIndex(typ, "/")+1:]
}

func (h *accessHarness) refresh() {
	h.t.Helper()
	if err := h.Runtime.Refresh(h.t.Context()); err != nil {
		h.t.Fatal(err)
	}
}

// authority is the key authority a gateway resolves for secret now.
func (h *accessHarness) authority(secret string) (access.Authority, error) {
	h.t.Helper()
	h.refresh()
	a, err := h.Runtime.Authenticate(secret)
	if err != nil {
		return access.Authority{}, err
	}
	return *a, nil
}

// gateway performs an SDK-style request against the inference surface.
func (h *accessHarness) gateway(method, path, key string, body any) (int, map[string]any, http.Header) {
	h.t.Helper()
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		payload = strings.NewReader(string(data))
	}
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.HTTP.URL+path, payload)
	if err != nil {
		h.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			h.t.Fatalf("%s %s: invalid JSON %q", method, path, raw)
		}
	}
	return resp.StatusCode, decoded, resp.Header
}

func (h *accessHarness) gatewayCode(status int, body map[string]any) string {
	h.t.Helper()
	e, _ := body["error"].(map[string]any)
	if e == nil {
		h.t.Fatalf("status %d without an OpenAI error envelope: %v", status, body)
	}
	code, _ := e["code"].(string)
	return code
}

// stream runs a streaming chat request and returns the concatenated text and
// whether the terminal marker arrived.
func (h *accessHarness) stream(key string) (string, bool, int) {
	h.t.Helper()
	body := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.HTTP.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false, resp.StatusCode
	}
	var text string
	done := false
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			h.t.Fatalf("bad chunk %q: %v", payload, err)
		}
		if chunk.Model != routeSlug {
			h.t.Fatalf("chunk model %q leaked the upstream identity", chunk.Model)
		}
		for _, c := range chunk.Choices {
			text += c.Delta.Content
		}
	}
	return text, done, resp.StatusCode
}

// Both flags are decided during composition, before anything is served,
// which is what setting them on a fresh harness stands in for here.

func TestGatewayFromEmptyInstallationToSDKTraffic(t *testing.T) {
	h := newAccessHarness(t)
	emitter := usage.NewEmitter(128)
	originalSink := h.Gateway.Sink
	h.Gateway.Sink = &gateway.AccountingSink{Emitter: emitter, Next: originalSink}
	owner := h.owner()
	up := newVendor(t)
	// This harness composes the surfaces without the shared state that
	// admission and the worker plane both need, so neither stored limits nor
	// retention are reported as enforced here.
	caps := h.want(nil, "GET", "/api/v1/auth/capabilities", nil, nil, 200)
	if caps["gateway_available"] != true || caps["limits_enforced"] != false || caps["retention_enforced"] != false {
		t.Fatalf("capabilities %v", caps)
	}
	if h.want(owner, "GET", "/api/v1/provider-kinds/openai_compatible/capabilities", nil, nil, 200)["provider_kind"] != "openai_compatible" {
		t.Fatal("kind capabilities")
	}
	h.want(owner, "GET", "/api/v1/provider-kinds/bedrock/capabilities", nil, nil, 200)

	// Unsafe egress is rejected at configuration time without any dispatch.
	configuration := func(endpoint string) map[string]any {
		return map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": endpoint}
	}
	unsafe := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Metadata", "configuration": configuration("http://169.254.169.254/latest"), "credential": vendorSecret}, map[string]string{"Idempotency-Key": "unsafe"}, 422)
	if problemCode(t, unsafe) != "validation_failed" {
		t.Fatalf("unsafe endpoint: %v", unsafe)
	}
	h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Unsupported", "configuration": map[string]any{"kind": "unsupported", "auth_mode": "api_key"}, "credential": "x"}, map[string]string{"Idempotency-Key": "unsupported"}, 422)

	// Certification and inference must agree on a base URL with a trailing slash.
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Fixture vendor", "configuration": configuration(up.URL + "/v1/"), "credential": vendorSecret}, map[string]string{"Idempotency-Key": "provider"}, 201)
	validateManagementResponse(t, "POST", "/api/v1/providers", 201, created)
	pid := created["id"].(string)
	providerPath := "/api/v1/providers/" + pid
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	validateManagementResponse(t, "GET", providerPath, 200, detail)
	if detail["state"] != "draft" || detail["connector_ready"] != true || detail["model_count"] != float64(0) {
		t.Fatalf("detail %v", detail)
	}
	if problemCode(t, h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "activate-empty"}), 422)) != "no_enabled_models" {
		t.Fatal("activation without models must fail")
	}

	// Probe, discover from upstream, enable capabilities.
	probe := h.want(owner, "POST", providerPath+"/probe", nil, etagHeader(detail), 200)
	validateManagementResponse(t, "POST", providerPath+"/probe", 200, probe)
	if probe["succeeded"] != true {
		t.Fatalf("probe %v", probe)
	}
	detail = h.want(owner, "POST", providerPath+"/discovery", map[string]any{"models": []any{}}, etagHeader(detail), 200)
	if detail["model_count"] != float64(2) {
		t.Fatalf("discovery %v", detail)
	}
	models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)
	validateManagementResponse(t, "GET", providerPath+"/models", 200, models)
	var modelID string
	for _, item := range models["items"].([]any) {
		m := item.(map[string]any)
		if m["upstream_model"] == vendorModel {
			modelID = m["id"].(string)
		}
		if m["enabled"] != false {
			t.Fatalf("discovered models must start disabled: %v", m)
		}
	}
	modelPath := providerPath + "/models/" + modelID
	capabilities := []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"}}
	if problemCode(t, h.want(owner, "PATCH", modelPath, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "image_generation", "surface": "openai", "mode": "unary"}}}, etagHeader(detail), 422)) != "capability_unavailable" {
		t.Fatal("unimplemented operation must stay unavailable")
	}
	detail = h.want(owner, "PATCH", modelPath, map[string]any{"enabled": true, "capabilities": capabilities}, etagHeader(detail), 200)
	if detail["enabled_model_count"] != float64(1) || detail["capability_count"] != float64(2) || detail["certified_capability_count"] != float64(0) {
		t.Fatalf("declared capabilities %v", detail)
	}
	if problemCode(t, h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "activate-declared"}), 422)) != "certification_required" {
		t.Fatal("declared capabilities must not activate")
	}

	// Certification records evidence per tuple; a failing upstream leaves it declared.
	up.fail.Store(true)
	failed := h.want(owner, "POST", modelPath+"/certify", nil, etagHeader(detail), 200)
	validateManagementResponse(t, "POST", modelPath+"/certify", 200, failed)
	if failed["status"] != "failed" || failed["certified_count"] != float64(0) || failed["attempted_count"] != float64(2) {
		t.Fatalf("failed certification %v", failed)
	}
	for _, result := range failed["results"].([]any) {
		if detailText := result.(map[string]any)["detail"].(string); strings.Contains(detailText, "boom") {
			t.Fatalf("probe content leaked into diagnostics: %q", detailText)
		}
	}
	up.fail.Store(false)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	certified := h.want(owner, "POST", modelPath+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" || certified["certified_count"] != float64(2) {
		t.Fatalf("certification %v", certified)
	}
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	if detail["certified_capability_count"] != float64(2) {
		t.Fatalf("evidence not retained %v", detail)
	}

	// Activation publishes a runtime generation.
	activation := h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "activate"}), 200)
	validateManagementResponse(t, "POST", providerPath+"/activate", 200, activation)
	if activation["state"] != "active" || activation["runtime_generation"].(map[string]any)["sequence"] != float64(1) {
		t.Fatalf("activation %v", activation)
	}
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	if detail["state"] != "active" || detail["pending_activation"] != false || detail["active_revision"] != float64(1) || detail["runtime_credential_version"] != float64(1) {
		t.Fatalf("active detail %v", detail)
	}

	// Route drafts: invalid targets cannot publish, stale edits cannot overwrite.
	target := map[string]any{"provider_id": pid, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}
	draftInput := map[string]any{"slug": routeSlug, "overall_timeout_ms": 5000, "max_attempts": 2, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{target}}
	unknown := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"slug": "bad", "overall_timeout_ms": 5000, "max_attempts": 2, "targets": []any{map[string]any{"provider_id": pid, "provider_model": "missing", "priority": 0, "weight": 1, "timeout_ms": 2000}}}, map[string]string{"Idempotency-Key": "draft-unknown"}, 422)
	if problemCode(t, unknown) != "validation_failed" {
		t.Fatalf("unknown model %v", unknown)
	}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", draftInput, map[string]string{"Idempotency-Key": "draft"}, 201)
	validateManagementResponse(t, "POST", "/api/v1/route-drafts", 201, draft)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	draftDetail := h.want(owner, "GET", draftPath, nil, nil, 200)
	validateManagementResponse(t, "GET", draftPath, 200, draftDetail)
	staleETag := etagHeader(draft)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draftDetail), 200)
	if validated["state"] != "validated" {
		t.Fatalf("validate %v", validated)
	}
	if problemCode(t, h.want(owner, "PUT", draftPath, map[string]any{"slug": routeSlug, "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 2, "targets": []any{}}, staleETag, 412)) != "etag_mismatch" {
		t.Fatal("stale draft edit must be rejected")
	}
	simulation := h.want(owner, "POST", draftPath+"/simulate", map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming", "seed": "tenant-a"}, nil, 200)
	validateManagementResponse(t, "POST", draftPath+"/simulate", 200, simulation)
	if targets := simulation["targets"].([]any); len(targets) != 1 || targets[0].(map[string]any)["eligible"] != true {
		t.Fatalf("simulation %v", simulation)
	}
	activated := h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "route-activate"}), 200)
	validateManagementResponse(t, "POST", draftPath+"/activate", 200, activated)
	routeID := activated["route_id"].(string)
	if activated["revision"] != float64(1) || activated["runtime_generation"].(map[string]any)["sequence"] != float64(2) {
		t.Fatalf("route activation %v", activated)
	}
	routes := h.want(owner, "GET", "/api/v1/routes", nil, nil, 200)
	validateManagementResponse(t, "GET", "/api/v1/routes", 200, routes)
	generations := h.want(owner, "GET", "/api/v1/runtime-generations", nil, nil, 200)
	validateManagementResponse(t, "GET", "/api/v1/runtime-generations", 200, generations)
	if len(generations["items"].([]any)) != 2 {
		t.Fatalf("generations %v", generations)
	}

	// Keys: explicit scopes and allowlists gate model listing and inference.
	fullKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "sdk", "scopes": []string{"inference", "models_read"}, "allowed_routes": []string{routeSlug}}, map[string]string{"Idempotency-Key": "key-full"}, 201)
	otherKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "other", "scopes": []string{"inference", "models_read"}, "allowed_routes": []string{"another-route"}}, map[string]string{"Idempotency-Key": "key-other"}, 201)
	readKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "read", "scopes": []string{"models_read"}}, map[string]string{"Idempotency-Key": "key-read"}, 201)
	unrestrictedKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "unrestricted", "scopes": []string{"inference", "models_read"}, "allowed_routes": []string{}}, map[string]string{"Idempotency-Key": "key-unrestricted"}, 201)
	secret, other, read := fullKey["secret"].(string), otherKey["secret"].(string), readKey["secret"].(string)
	if status, body, _ := h.gateway("GET", "/v1/models", secret, nil); status != 503 || h.gatewayCode(status, body) != "authority_unavailable" {
		t.Fatalf("authority must be loaded before admission: %d %v", status, body)
	}
	h.refresh()
	if release := h.Runtime.Release(); release.Sequence != 2 || len(release.Snapshot.Routes) != 1 {
		t.Fatalf("release %+v", release)
	}
	if status, body, _ := h.gateway("GET", "/v1/models", secret, nil); status != 200 || len(body["data"].([]any)) != 1 {
		t.Fatalf("models %d %v", status, body)
	}
	if status, body, _ := h.gateway("GET", "/v1/models", other, nil); status != 200 || len(body["data"].([]any)) != 0 {
		t.Fatalf("filtered models %d %v", status, body)
	}
	if status, body, _ := h.gateway("GET", "/v1/models/"+routeSlug, other, nil); status != 404 || h.gatewayCode(status, body) != "route_not_found" {
		t.Fatalf("filtered retrieval %d %v", status, body)
	}
	chat := map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for _, tc := range []struct {
		key      map[string]any
		eligible bool
	}{{fullKey, true}, {unrestrictedKey, true}, {otherKey, false}} {
		decisions := h.list(owner, "POST", "/api/v1/routing/simulate", map[string]any{"operation": map[string]any{"operation": "generation", "request": map[string]any{"route": routeSlug}}, "surface": "openai", "mode": "unary", "seed": "tenant-a", "api_key_id": tc.key["id"]}, nil, 200)
		if len(decisions) != 1 {
			t.Fatalf("simulation for key %v: %v", tc.key["id"], decisions)
		}
		decision := decisions[0].(map[string]any)
		if decision["eligible"] != tc.eligible || (!tc.eligible && decision["reason"] != "route_not_allowed_for_key") {
			t.Fatalf("simulation disagrees with key routes: %v", decision)
		}
	}
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", unrestrictedKey["secret"].(string), chat); status != 200 {
		t.Fatalf("unrestricted key inference: %d %v", status, body)
	}
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", read, chat); status != 403 || h.gatewayCode(status, body) != "permission_denied" {
		t.Fatalf("scope rejection %d %v", status, body)
	}
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", other, chat); status != 403 || h.gatewayCode(status, body) != "route_forbidden" {
		t.Fatalf("allowlist rejection %d %v", status, body)
	}
	status, body, headers := h.gateway("POST", "/v1/chat/completions", secret, chat)
	if status != 200 || body["model"] != routeSlug || headers.Get("X-Request-Id") == "" {
		t.Fatalf("unary %d %v", status, body)
	}
	if text := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]; text != vendorAnswer {
		t.Fatalf("content %v", text)
	}
	if text, done, status := h.stream(secret); text != vendorAnswer || !done || status != 200 {
		t.Fatalf("stream %q done=%v status=%d", text, done, status)
	}
	responses := map[string]any{"model": routeSlug, "input": "hi"}
	if status, body, _ := h.gateway("POST", "/v1/responses", secret, responses); status != 200 || body["status"] != "completed" || body["model"] != routeSlug {
		t.Fatalf("certified Responses request: %d %v", status, body)
	}

	// Playground uses the same runtime through the console session.
	h.want(owner, "POST", "/api/v1/playground", map[string]any{"model": routeSlug, "input": "hi", "routing": map[string]any{"strategy": "unknown"}}, nil, 422)
	h.want(owner, "POST", "/api/v1/playground", map[string]any{"model": routeSlug, "input": "hi", "routing": map[string]any{"only": []string{"vendor"}}}, nil, 422)
	play := h.want(owner, "POST", "/api/v1/playground", map[string]any{"model": routeSlug, "input": "hi", "routing": map[string]any{"strategy": "weighted", "allow_fallbacks": false}}, nil, 200)
	validateManagementResponse(t, "POST", "/api/v1/playground", 200, play)
	if play["output_text"] != vendorAnswer || play["model"] != routeSlug || len(play["routing"].([]any)) != 1 {
		t.Fatalf("playground %v", play)
	}
	// Health reads durable attempt facts. Drain this fixture's accounting
	// events directly into PostgreSQL; the replica fleet tests cover
	// distributed delivery.
	h.Gateway.Sink = originalSink
	emitter.Close()
	flush, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	emitter.RunWriter(flush, persistedUsageStream{h.Pool}, "fixture", slog.Default())
	cancel()
	if snapshot := emitter.Snapshot(); snapshot.Persisted < 3 || snapshot.Lost() != 0 {
		t.Fatalf("health accounting did not persist: %+v", snapshot)
	}
	health := h.want(owner, "GET", "/api/v1/provider-health", nil, nil, 200)
	validateManagementResponse(t, "GET", "/api/v1/provider-health", 200, health)
	if items := health["items"].([]any); len(items) != 1 || items[0].(map[string]any)["attempt_count"].(float64) < 3 {
		t.Fatalf("health %v", health)
	}
	// Each attempt carries the estimate it was admitted under through the event
	// stream into storage: the fixture model has no public tokenizer, so its
	// input is counted by the four-characters rule, and the family and method
	// say so.
	var attempts, estimated int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),
	    count(*) FILTER (WHERE estimate_provenance = 'heuristic' AND model_family = 'other' AND estimated_input_tokens > 0)
	    FROM olp.attempt_usage_facts`).Scan(&attempts, &estimated); err != nil {
		t.Fatal(err)
	}
	if attempts < 3 || estimated != attempts {
		t.Fatalf("%d of %d attempts recorded their estimate", estimated, attempts)
	}

	// Draft edits never change serving traffic until activation.
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	draftConfiguration := detail["configuration"].(map[string]any)
	draftConfiguration["options"].(map[string]any)["parameter_defaults"] = map[string]any{"temperature": 0.2}
	edited := h.want(owner, "PATCH", providerPath, map[string]any{"name": "Fixture vendor (edited)", "configuration": draftConfiguration}, etagHeader(detail), 200)
	if edited["pending_activation"] != true || edited["certified_capability_count"] != float64(0) {
		t.Fatalf("transport change must invalidate evidence: %v", edited)
	}
	h.refresh()
	if status, _, _ := h.gateway("POST", "/v1/chat/completions", secret, chat); status != 200 {
		t.Fatalf("draft edit changed serving traffic: %d", status)
	}
	restored := h.want(owner, "POST", providerPath+"/restore-as-draft", nil, withMatch(edited, map[string]string{"Idempotency-Key": "restore"}), 200)
	if restored["certified_capability_count"] != float64(2) || restored["pending_activation"] != false {
		t.Fatalf("restore %v", restored)
	}

	// In-flight publication changes: a slow stream keeps its pinned snapshot
	// while the provider is disabled underneath it.
	up.delay.Store(int64(120 * time.Millisecond))
	streamText := make(chan string, 1)
	go func() {
		text, done, _ := h.stream(secret)
		if !done {
			text = "INCOMPLETE:" + text
		}
		streamText <- text
	}()
	time.Sleep(150 * time.Millisecond)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	disabled := h.want(owner, "POST", providerPath+"/disable", nil, withMatch(detail, map[string]string{"Idempotency-Key": "disable"}), 200)
	validateManagementResponse(t, "POST", providerPath+"/disable", 200, disabled)
	h.refresh()
	if text := <-streamText; text != vendorAnswer {
		t.Fatalf("in-flight stream broken by publication: %q", text)
	}
	up.delay.Store(0)
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", secret, chat); status != 503 || h.gatewayCode(status, body) != "upstream_unavailable" {
		t.Fatalf("disabled provider still served: %d %v", status, body)
	}
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "reactivate"}), 200)
	h.refresh()
	if status, _, _ := h.gateway("POST", "/v1/chat/completions", secret, chat); status != 200 {
		t.Fatalf("re-activation did not restore traffic: %d", status)
	}

	// Revocation: keys and credential versions are authority state, not releases.
	keyPath := "/api/v1/api-keys/" + fullKey["id"].(string)
	keyRecord := h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "POST", keyPath+"/revoke", nil, withMatch(keyRecord, map[string]string{"Idempotency-Key": "key-revoke"}), 200)
	if status, _, _ := h.gateway("POST", "/v1/chat/completions", secret, chat); status != 200 {
		t.Fatalf("pinned authority must serve until refresh: %d", status)
	}
	h.refresh()
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", secret, chat); status != 401 || h.gatewayCode(status, body) != "invalid_api_key" {
		t.Fatalf("revoked key %d %v", status, body)
	}
	replacement := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "sdk-2", "scopes": []string{"inference", "models_read"}}, map[string]string{"Idempotency-Key": "key-2"}, 201)["secret"].(string)
	h.refresh()
	credentials := h.want(owner, "GET", providerPath+"/credentials", nil, nil, 200)
	validateManagementResponse(t, "GET", providerPath+"/credentials", 200, credentials)
	credential := credentials["items"].([]any)[0].(map[string]any)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	revoked := h.want(owner, "POST", providerPath+"/credentials/"+credential["id"].(string)+"/revoke", nil, withMatch(detail, map[string]string{"Idempotency-Key": "cred-revoke"}), 200)
	validateManagementResponse(t, "POST", providerPath+"/credentials/"+credential["id"].(string)+"/revoke", 200, revoked)
	h.refresh()
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", replacement, chat); status != 503 || h.gatewayCode(status, body) != "upstream_unavailable" {
		t.Fatalf("revoked credential still selectable: %d %v", status, body)
	}
	// Rotation validates the new secret upstream and does not touch the pinned revision.
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	if problemCode(t, h.want(owner, "POST", providerPath+"/credentials", map[string]any{"credential": "not-accepted"}, withMatch(detail, map[string]string{"Idempotency-Key": "rotate-bad"}), 422)) != "credential_invalid" {
		t.Fatal("rotation must validate the credential")
	}
	up.accept(vendorRotated)
	rotateHeaders := withMatch(detail, map[string]string{"Idempotency-Key": "rotate"})
	rotateInput := map[string]any{"credential": vendorRotated}
	rotated := h.want(owner, "POST", providerPath+"/credentials", rotateInput, rotateHeaders, 201)
	validateManagementResponse(t, "POST", providerPath+"/credentials", 201, rotated)
	// A lost-response retry replays the original ETag and body even if the
	// upstream is now unavailable, without probing or storing another version.
	modelCalls := up.models.Load()
	up.fail.Store(true)
	replayed := h.want(owner, "POST", providerPath+"/credentials", rotateInput, rotateHeaders, 201)
	up.fail.Store(false)
	if !reflect.DeepEqual(replayed, rotated) || up.models.Load() != modelCalls {
		t.Fatalf("rotation retry did not replay: %v, original %v, probes %d -> %d", replayed, rotated, modelCalls, up.models.Load())
	}
	if problemCode(t, h.want(owner, "POST", providerPath+"/credentials", map[string]any{"credential": "different"}, rotateHeaders, 409)) != "idempotency_conflict" {
		t.Fatal("rotation replay accepted a different credential")
	}
	if problemCode(t, h.want(owner, "POST", providerPath+"/credentials", rotateInput, withMatch(rotated, map[string]string{"Idempotency-Key": "rotate"}), 409)) != "idempotency_conflict" {
		t.Fatal("rotation replay accepted a different precondition")
	}
	if problemCode(t, h.want(owner, "POST", providerPath+"/credentials", rotateInput, withMatch(detail, map[string]string{"Idempotency-Key": "rotate-stale"}), 412)) != "etag_mismatch" {
		t.Fatal("a new rotation accepted a stale ETag")
	}
	credentials = h.want(owner, "GET", providerPath+"/credentials", nil, nil, 200)
	if len(credentials["items"].([]any)) != 2 {
		t.Fatalf("rotation retry created another credential: %v", credentials)
	}
	h.refresh()
	if status, _, _ := h.gateway("POST", "/v1/chat/completions", replacement, chat); status != 503 {
		t.Fatalf("rotation substituted a credential into the pinned revision: %d", status)
	}
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	if detail["draft_credential_version"] != float64(2) || detail["runtime_credential_version"] != float64(1) {
		t.Fatalf("credential versions %v", detail)
	}
	detail = h.want(owner, "POST", providerPath+"/restore-as-draft", nil, withMatch(detail, map[string]string{"Idempotency-Key": "restore-with-rotated-credential"}), 200)
	if detail["pending_activation"] != true || detail["draft_credential_version"] != float64(2) || detail["runtime_credential_version"] != float64(1) {
		t.Fatalf("restore hid the unpublished credential: %v", detail)
	}
	revisionRestorePath := providerPath + "/revisions/" + fmt.Sprint(int(detail["active_revision"].(float64))) + "/restore-as-draft"
	revisionRestored := h.want(owner, "POST", revisionRestorePath, nil, withMatch(detail, map[string]string{"Idempotency-Key": "restore-revision-with-rotated-credential"}), 200)
	detail = revisionRestored["provider"].(map[string]any)
	if revisionRestored["credential_restored"] != false || detail["pending_activation"] != true || detail["draft_credential_version"] != float64(2) || detail["runtime_credential_version"] != float64(1) {
		t.Fatalf("revision restore hid the unpublished credential: %v", revisionRestored)
	}
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "activate-rotated"}), 200)
	h.refresh()
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", replacement, chat); status != 200 {
		t.Fatalf("rotated credential not served after activation: %d %v", status, body)
	}

	// History: immutable revisions, diffs, and restoring a route revision as a draft.
	revisions := h.want(owner, "GET", providerPath+"/revisions", nil, nil, 200)
	validateManagementResponse(t, "GET", providerPath+"/revisions", 200, revisions)
	if len(revisions["items"].([]any)) != 3 {
		t.Fatalf("provider revisions %v", revisions)
	}
	diff := h.want(owner, "GET", providerPath+"/revisions/diff?from=1&to=3", nil, nil, 200)
	validateManagementResponse(t, "GET", providerPath+"/revisions/diff", 200, diff)
	if diff["credential_changed"] != true || diff["endpoint_changed"] != false {
		t.Fatalf("diff %v", diff)
	}
	routeRevisions := h.want(owner, "GET", "/api/v1/routes/"+routeID+"/revisions", nil, nil, 200)
	validateManagementResponse(t, "GET", "/api/v1/routes/"+routeID+"/revisions", 200, routeRevisions)
	revisionID := routeRevisions["items"].([]any)[0].(map[string]any)["id"].(string)
	restoredDraft := h.want(owner, "POST", "/api/v1/routes/"+routeID+"/revisions/"+revisionID+"/restore-as-draft", nil, map[string]string{"Idempotency-Key": "route-restore"}, 201)
	validateManagementResponse(t, "POST", "/api/v1/routes/"+routeID+"/revisions/"+revisionID+"/restore-as-draft", 201, restoredDraft)
	if restoredDraft["based_on_revision_id"] != revisionID || restoredDraft["slug"] != routeSlug {
		t.Fatalf("restored draft %v", restoredDraft)
	}
	decisions := h.list(owner, "POST", "/api/v1/routing/simulate", map[string]any{"operation": map[string]any{"operation": "generation", "request": map[string]any{"route": routeSlug}}, "surface": "openai", "mode": "unary", "seed": "tenant-a"}, nil, 200)
	if len(decisions) != 1 {
		t.Fatalf("decisions %v", decisions)
	}
	if decision := decisions[0].(map[string]any); decision["eligible"] != true || decision["attempt"] != float64(1) || decision["upstream_model"] != vendorModel || decision["strategy"] != "weighted" {
		t.Fatalf("decision %v", decision)
	}
	if up.cancels.Load() != 0 {
		t.Fatalf("vendor observed %d cancellations", up.cancels.Load())
	}
}

func TestGatewayToolWorkflowSettlesOneRun(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Gateway workflow")
	const argument = "workflow-private-argument"
	const answer = "workflow-private-result"
	const prompt = "workflow-private-prompt"
	var toolCalls atomic.Int64
	var responsesWorkflow atomic.Bool
	var blockTool atomic.Bool
	var deadlineTool atomic.Bool
	deadlineStarted := make(chan struct{}, 2)
	deadlineCanceled := make(chan struct{}, 2)
	toolStarted := make(chan struct{})
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch request.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "workflow-session")
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			schema := map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}}
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "inputSchema": schema}, map[string]any{"name": "change", "inputSchema": schema}}}
		case "tools/call":
			if deadlineTool.Load() {
				deadlineStarted <- struct{}{}
				<-r.Context().Done()
				deadlineCanceled <- struct{}{}
				return
			}
			if blockTool.Load() {
				close(toolStarted)
				<-r.Context().Done()
				return
			}
			if !responsesWorkflow.Load() {
				var settledModels int
				if err := h.Pool.QueryRow(r.Context(), `SELECT count(*) FROM olp.execution_debits WHERE kind='model' AND state='settled'`).Scan(&settledModels); err != nil || settledModels != 1 {
					t.Errorf("tool dispatched before model settlement: settled=%d error=%v", settledModels, err)
				}
			}
			toolCalls.Add(1)
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}}, "isError": false}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer toolServer.Close()
	registered := h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{"name": "Workflow tools", "project_id": project, "transport": "streamable_http", "endpoint": toolServer.URL, "enabled": true, "unit_price": "0.25"}, idem("workflow-server"), 201)
	serverID := mcpExecString(t, registered, "id")
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, registered, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, registered, "catalog"), "digest"), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup"), Classification: "read_only"}
	approvalTool := agentcontract.Tool{ServerID: serverID, RevisionID: tool.RevisionID, CatalogDigest: tool.CatalogDigest, Name: "change", Alias: agentcontract.ToolAlias(serverID, "change")}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{"project_id": project, "name": "Workflow toolset", "toolset": agentcontract.Toolset{Tools: []agentcontract.Tool{tool, approvalTool}}}, idem("workflow-toolset"), 201)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("workflow-publish")), 200)

	var modelCalls atomic.Int64
	var noUsage atomic.Bool
	var blockModel atomic.Bool
	var deadlineModel atomic.Bool
	var blockRun atomic.Bool
	runStarted := make(chan struct{}, 2)
	runRelease := make(chan struct{})
	defer close(runRelease)
	modelStarted := make(chan struct{})
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
			writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"id": vendorModel}}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/responses") {
			var request struct {
				Input json.RawMessage `json:"input"`
				Tools json.RawMessage `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if strings.Contains(string(request.Tools), "olp_certification_probe") {
				writeJSON(w, map[string]any{"id": "response-probe", "object": "response", "model": vendorModel, "status": "completed", "output": []any{map[string]any{"id": "function-probe", "type": "function_call", "call_id": "call-probe", "name": "olp_certification_probe", "arguments": `{}`}}, "usage": map[string]int{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}})
				return
			}
			if responsesWorkflow.Load() {
				if !strings.Contains(string(request.Input), `"function_call_output"`) {
					writeJSON(w, map[string]any{"id": "response-tool", "object": "response", "model": vendorModel, "status": "completed", "output": []any{map[string]any{"id": "function-call", "type": "function_call", "call_id": "call-one", "name": tool.Alias, "arguments": `{"query":"` + argument + `"}`}}, "usage": map[string]int{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}})
					return
				}
			}
			writeResponsesFixture(w, vendorModel, "done", false)
			return
		}
		var input map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&input) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if blockRun.Load() && len(input["tool_choice"]) == 0 {
			runStarted <- struct{}{}
			select {
			case <-runRelease:
			case <-r.Context().Done():
				return
			}
		}
		if deadlineModel.Load() && len(input["tool_choice"]) == 0 {
			deadlineStarted <- struct{}{}
			<-r.Context().Done()
			deadlineCanceled <- struct{}{}
			return
		}
		if blockModel.Load() && len(input["tool_choice"]) == 0 {
			close(modelStarted)
			<-r.Context().Done()
			return
		}
		name := ""
		if len(input["tool_choice"]) > 0 {
			name = "olp_certification_probe"
		} else if len(input["tools"]) > 0 {
			modelCalls.Add(1)
			if !strings.Contains(string(input["messages"]), `"role":"tool"`) {
				name = tool.Alias
				if strings.Contains(string(input["messages"]), "workflow-approval-test") {
					name = approvalTool.Alias
				}
			} else {
				var settledModels, settledTools int
				if err := h.Pool.QueryRow(r.Context(), `SELECT count(*) FILTER (WHERE kind='model' AND state='settled'),count(*) FILTER (WHERE kind='tool' AND state='settled') FROM olp.execution_debits`).Scan(&settledModels, &settledTools); err != nil || settledModels != 1 || settledTools != 1 {
					t.Errorf("next model dispatched before previous steps settled: models=%d tools=%d error=%v", settledModels, settledTools, err)
				}
			}
		}
		message := map[string]any{"role": "assistant", "content": "done"}
		finish := "stop"
		if name != "" {
			finish = "tool_calls"
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call-one", "type": "function", "function": map[string]any{"name": name, "arguments": `{"query":"` + argument + `"}`}}}}
		}
		body := map[string]any{"id": "chatcmpl-workflow", "object": "chat.completion", "created": 1, "model": vendorModel, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}}
		if !noUsage.Load() || len(input["tools"]) == 0 || len(input["tool_choice"]) > 0 {
			body["usage"] = map[string]any{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer vendor.Close()
	_, provider, slug, _ := provisionOpenAIInProject(t, h, owner, &project, vendor.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"},
		map[string]any{"tool_execution": "gateway", "toolsets": []string{toolsetID}, "responses_mcp_items": true, "budget": map[string]any{"daily_cost_limit": "10000000"}, "run_limits": map[string]any{"max_tool_calls": 3, "max_duration_seconds": 600, "max_concurrent_runs": 3}},
		map[string]any{"options": map[string]any{"models": map[string]any{vendorModel: map[string]any{"context_length": 1000}}}})
	addBatchPrice(t, h, owner, provider["id"].(string), "", time.Now().Add(-time.Hour))
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Workflow caller", "project_id": project, "scopes": []string{"inference", "tools", "traces_write"}, "allowed_routes": []string{slug}, "allowed_toolsets": []string{toolsetID}}, idem("workflow-key"), 201)
	keyID, secret := mcpExecString(t, key, "id"), mcpExecString(t, key, "secret")
	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	limiter.UseDurableDebits(h.Pool)
	store := &limits.DebitStore{Pool: h.Pool, Limiter: limiter}
	h.Gateway.Admission = gateway.NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
	h.Gateway.Debits = gateway.DurableSessionDebits(store)
	h.Gateway.WorkflowRuns = &agents.WorkflowStore{Pool: h.Pool, Auth: h.AuthKey, Debits: store}
	h.Gateway.WorkflowTools = &mcp.Server{Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Limiter: limiter, Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: store, Auth: h.AuthKey}}
	emitter := usage.NewEmitter(8)
	h.Gateway.Sink = &gateway.AccountingSink{Emitter: emitter}
	h.refresh()
	leader, err := limits.TryAcquireLeader(t.Context(), h.Pool)
	if err != nil || leader == nil {
		t.Fatalf("acquire cost reconciliation leader: %v", err)
	}
	if _, err := leader.Reconcile(t.Context(), limiter, time.Now()); err != nil {
		leader.Close(t.Context())
		t.Fatalf("reconcile workflow cost caps: %v", err)
	}
	leader.Close(t.Context())
	status, response, _ := h.gateway("POST", "/v1/chat/completions", secret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": prompt}}, "max_tokens": 100})
	if status != http.StatusOK || modelCalls.Load() != 2 || toolCalls.Load() != 1 || !strings.Contains(fmt.Sprint(response), "done") {
		t.Fatalf("workflow did not finish: status=%d response=%v model_calls=%d tool_calls=%d", status, response, modelCalls.Load(), toolCalls.Load())
	}
	var runID string
	var steps, models, tools, settled, distinctRuns int
	if err := h.Pool.QueryRow(t.Context(), `SELECT min(run_id::text),count(*),count(*) FILTER(WHERE kind='model'),count(*) FILTER(WHERE kind='tool'),count(*) FILTER(WHERE state='settled'),count(DISTINCT run_id) FROM olp.execution_debits WHERE api_key_id=$1`, keyID).Scan(&runID, &steps, &models, &tools, &settled, &distinctRuns); err != nil {
		t.Fatal(err)
	}
	if steps != 3 || models != 2 || tools != 1 || settled != 3 || distinctRuns != 1 {
		t.Fatalf("workflow did not settle distinct steps under one run: steps=%d models=%d tools=%d settled=%d runs=%d", steps, models, tools, settled, distinctRuns)
	}
	var order string
	if err := h.Pool.QueryRow(t.Context(), `SELECT string_agg(s.ordinal::text || ':' || d.kind, ',' ORDER BY s.ordinal) FROM olp.agent_steps s JOIN olp.execution_debits d ON d.id=s.dispatch_id WHERE s.run_id=$1`, runID).Scan(&order); err != nil {
		t.Fatal(err)
	}
	if order != "0:model,1:tool,2:model" {
		t.Fatalf("workflow did not persist each dispatch in order: %s", order)
	}
	var before, after string
	if err := h.Pool.QueryRow(t.Context(), `SELECT COALESCE(sum(accrued),0)::text FROM olp.api_key_cost_windows WHERE api_key_id=$1 AND window_kind='day'`, keyID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	emitter.Close()
	emitter.RunWriter(t.Context(), persistedUsageStream{h.Pool}, "workflow", slog.Default())
	if err := h.Pool.QueryRow(t.Context(), `SELECT COALESCE(sum(accrued),0)::text FROM olp.api_key_cost_windows WHERE api_key_id=$1 AND window_kind='day'`, keyID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("terminal receipt changed settled spend: before=%s after=%s", before, after)
	}
	var keyMatches, positiveCharge bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT COALESCE(sum(observed_charge),0)=$2::numeric, COALESCE(sum(observed_charge),0)>0 FROM olp.execution_debits WHERE run_id=$1`, runID, after).Scan(&keyMatches, &positiveCharge); err != nil {
		t.Fatal(err)
	}
	if !keyMatches || !positiveCharge {
		t.Fatalf("key spend did not equal the settled workflow charges: spend=%s matches=%t positive=%t", after, keyMatches, positiveCharge)
	}
	var supplyMatches bool
	var supplyOwners int
	if err := h.Pool.QueryRow(t.Context(), `SELECT
		(SELECT COALESCE(sum(w.accrued),0) FROM olp.supply_cost_windows w JOIN olp.route_revisions rr ON rr.route_id=w.owner_id JOIN olp.agent_runs r ON r.route_revision_id=rr.id WHERE r.id=$1 AND w.window_kind='day') =
		(SELECT COALESCE(sum(d.observed_cost),0) FROM olp.execution_debits d WHERE d.run_id=$1 AND d.kind='model'),
		(SELECT count(*) FROM olp.execution_debit_owners o JOIN olp.execution_debits d ON d.id=o.dispatch_id JOIN olp.agent_runs r ON r.id=d.run_id JOIN olp.route_revisions rr ON rr.id=r.route_revision_id WHERE d.run_id=$1 AND d.kind='model' AND o.owner_kind='supply' AND o.basis='cost' AND o.owner_id=rr.route_id)`, runID).Scan(&supplyMatches, &supplyOwners); err != nil {
		t.Fatal(err)
	}
	if !supplyMatches || supplyOwners != 2 {
		t.Fatalf("supply settlement differs from model costs: matches=%t debit owners=%d", supplyMatches, supplyOwners)
	}
	var facts, counted, receipts int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER(WHERE request_counted) FROM olp.attempt_usage_facts WHERE event_id=$1`, runID).Scan(&facts, &counted); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.request_metadata_event_receipts WHERE event_id=$1`, runID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if facts != 2 || counted != 1 || receipts != 1 {
		t.Fatalf("workflow accounting lost or duplicated request data: facts=%d counted=%d receipts=%d", facts, counted, receipts)
	}
	verifyWorkflowTraceCorrelation(t, h, secret, keyID, runID, limiter)
	var stored string
	if err := h.Pool.QueryRow(t.Context(), `SELECT
		COALESCE((SELECT row_to_json(r)::text FROM olp.agent_runs r WHERE r.id=$1),'') ||
		COALESCE((SELECT string_agg(row_to_json(s)::text,'') FROM olp.agent_steps s WHERE s.run_id=$1),'') ||
		COALESCE((SELECT string_agg(row_to_json(d)::text,'') FROM olp.execution_debits d WHERE d.run_id=$1),'') ||
		COALESCE((SELECT string_agg(row_to_json(c)::text,'') FROM olp.mcp_tool_calls c JOIN olp.execution_debits d ON d.id=c.dispatch_id WHERE d.run_id=$1),'') ||
		COALESCE((SELECT string_agg(row_to_json(f)::text,'') FROM olp.attempt_usage_facts f WHERE f.event_id=$1),'')`, runID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, prompt) || strings.Contains(stored, argument) || strings.Contains(stored, answer) {
		t.Fatal("workflow stored conversation or tool content in its run or attempt records")
	}
	noUsage.Store(true)
	h.Gateway.Sink = &gateway.AccountingSink{Emitter: usage.NewEmitter(8)}
	status, response, _ = h.gateway("POST", "/v1/chat/completions", secret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": prompt}}, "max_tokens": 100})
	if status != http.StatusServiceUnavailable || modelCalls.Load() != 3 || toolCalls.Load() != 1 {
		t.Fatalf("workflow continued after missing model usage: status=%d response=%v model_calls=%d tool_calls=%d", status, response, modelCalls.Load(), toolCalls.Load())
	}
	var unknown int
	var exposureRetained bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),COALESCE(bool_and(closed_at IS NULL AND charge_bound>0),false) FROM olp.execution_debits WHERE api_key_id=$1 AND state='outcome_unknown'`, keyID).Scan(&unknown, &exposureRetained); err != nil {
		t.Fatal(err)
	}
	if unknown != 1 || !exposureRetained {
		t.Fatalf("missing usage did not retain model exposure: unknown=%d retained=%t", unknown, exposureRetained)
	}
	noUsage.Store(false)
	h.Gateway.Sink = &gateway.AccountingSink{Emitter: usage.NewEmitter(8)}
	status, response, _ = h.gateway("POST", "/v1/chat/completions", secret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "workflow-approval-test"}}, "max_tokens": 100})
	if status != http.StatusForbidden || !strings.Contains(fmt.Sprint(response), "tool_approval_required") || modelCalls.Load() != 4 || toolCalls.Load() != 1 {
		t.Fatalf("unclassified tool was not refused before dispatch: status=%d response=%v model_calls=%d tool_calls=%d", status, response, modelCalls.Load(), toolCalls.Load())
	}

	unavailable := httptest.NewServer(vendor.Config.Handler)
	unavailableProvider := activeAzureProviderInProject(t, h, owner, "Unavailable workflow provider", unavailable.URL, &project, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	const fallbackSlug = "workflow-fallback"
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"project_id": project, "slug": fallbackSlug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 2,
		"fidelity": map[string]any{"mode": "transformed"}, "tool_execution": "gateway",
		"targets": []any{
			map[string]any{"provider_id": unavailableProvider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000},
			map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 1, "weight": 1, "timeout_ms": 5000},
		},
	}, idem("workflow-fallback-draft"), http.StatusCreated)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("workflow-fallback-activate")), http.StatusOK)
	fallbackKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Workflow fallback", "project_id": project, "scopes": []string{"inference"}, "allowed_routes": []string{fallbackSlug}}, idem("workflow-fallback-key"), http.StatusCreated)
	unavailable.Close()
	h.refresh()
	h.Gateway.Sink = &gateway.AccountingSink{Emitter: usage.NewEmitter(8)}
	status, response, _ = h.gateway("POST", "/v1/chat/completions", mcpExecString(t, fallbackKey, "secret"), map[string]any{"model": fallbackSlug, "messages": []any{map[string]any{"role": "user", "content": prompt}}, "max_tokens": 100})
	if status != http.StatusOK || !strings.Contains(fmt.Sprint(response), "done") {
		t.Fatalf("workflow did not fall back after an unsent attempt: status=%d response=%v", status, response)
	}
	var notSent, fallbackSettled int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE state='not_accepted' AND closed_at IS NOT NULL),count(*) FILTER (WHERE state='settled') FROM olp.execution_debits WHERE api_key_id=$1`, fallbackKey["id"]).Scan(&notSent, &fallbackSettled); err != nil {
		t.Fatal(err)
	}
	if notSent != 1 || fallbackSettled != 1 {
		t.Fatalf("unsent debit remained open: not_accepted=%d settled=%d", notSent, fallbackSettled)
	}

	const revocationSlug = "workflow-revocation"
	revocationDraft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"project_id": project, "slug": revocationSlug, "operations": []string{"generation"}, "overall_timeout_ms": 20000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "transformed"}, "tool_execution": "gateway", "toolsets": []string{toolsetID},
		"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 15000}},
	}, idem("workflow-revocation-draft"), http.StatusCreated)
	h.want(owner, "POST", "/api/v1/route-drafts/"+revocationDraft["id"].(string)+"/activate", nil, withMatch(revocationDraft, idem("workflow-revocation-activate")), http.StatusOK)
	for _, scenario := range []struct {
		name    string
		block   *atomic.Bool
		started <-chan struct{}
	}{
		{name: "model", block: &blockModel, started: modelStarted},
		{name: "tool", block: &blockTool, started: toolStarted},
	} {
		revocationKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Workflow revocation " + scenario.name, "project_id": project, "scopes": []string{"inference", "tools"}, "allowed_routes": []string{revocationSlug}, "allowed_toolsets": []string{toolsetID}}, idem("workflow-revocation-key-"+scenario.name), http.StatusCreated)
		h.refresh()
		h.Gateway.Sink = &gateway.AccountingSink{Emitter: usage.NewEmitter(8)}
		scenario.block.Store(true)
		result := make(chan int, 1)
		go func() {
			status, _, _ := h.gateway("POST", "/v1/chat/completions", mcpExecString(t, revocationKey, "secret"), map[string]any{"model": revocationSlug, "messages": []any{map[string]any{"role": "user", "content": prompt}}, "max_tokens": 100})
			result <- status
		}()
		select {
		case <-scenario.started:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s dispatch did not start", scenario.name)
		}
		current := h.want(owner, "GET", "/api/v1/api-keys/"+revocationKey["id"].(string), nil, nil, http.StatusOK)
		h.want(owner, "POST", "/api/v1/api-keys/"+revocationKey["id"].(string)+"/revoke", nil, withMatch(current, idem("workflow-revoke-"+scenario.name)), http.StatusOK)
		h.refresh()
		select {
		case status := <-result:
			if status != http.StatusForbidden {
				t.Fatalf("revoked %s call returned status %d", scenario.name, status)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("revoked %s call was not cancelled", scenario.name)
		}
		scenario.block.Store(false)
		var unknown int
		var retained bool
		if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),COALESCE(bool_and(closed_at IS NULL),false) FROM olp.execution_debits WHERE api_key_id=$1 AND kind=$2 AND state='outcome_unknown'`, revocationKey["id"], scenario.name).Scan(&unknown, &retained); err != nil {
			t.Fatal(err)
		}
		if unknown != 1 || !retained {
			t.Fatalf("revoked %s call did not retain unknown exposure: count=%d retained=%t", scenario.name, unknown, retained)
		}
	}
	controlSlug := "workflow-capture-control"
	_, projectedProvider, projectedSlug, _ := provisionOpenAIInProject(t, h, owner, &project, vendor.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"},
		map[string]any{"tool_execution": "gateway", "toolsets": []string{toolsetID}, "responses_mcp_items": true},
		map[string]any{"kind": "openai", "profile_id": "openai-responses", "profile_revision": "1", "deployment": nil, "api_version": nil})
	h.want(owner, http.MethodPost, "/api/v1/pricing/revisions", map[string]any{
		"effective_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		"prices": []any{map[string]any{"provider_kind": "openai", "provider_id": projectedProvider["id"], "model": vendorModel,
			"operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000"}},
	}, idem("workflow-projected-price"), http.StatusCreated)
	eventually(t, "projected workflow price in routing inputs", 30*time.Second, func() bool {
		h.refresh()
		inputs := h.Runtime.RoutingInputs()
		return inputs != nil && inputs.Price("openai", projectedProvider["id"].(string), "", vendorModel, "generation", time.Now()) != nil
	})
	projectedKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Workflow capture projected", "project_id": project, "scopes": []string{"inference", "tools"}, "allowed_routes": []string{projectedSlug}, "allowed_toolsets": []string{toolsetID}}, idem("workflow-capture-projected-key"), http.StatusCreated)
	controlDraft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
		"project_id": project, "slug": controlSlug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "transformed"},
		"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, idem("workflow-capture-draft"), http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+controlDraft["id"].(string)+"/activate", nil, withMatch(controlDraft, idem("workflow-capture-activate")), http.StatusOK)
	controlKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Workflow capture control", "project_id": project, "scopes": []string{"inference"}, "allowed_routes": []string{controlSlug}}, idem("workflow-capture-key"), http.StatusCreated)
	finalOnlySlug := "workflow-capture-final-only"
	finalOnlyDraft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
		"project_id": project, "slug": finalOnlySlug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "transformed"}, "tool_execution": "gateway", "toolsets": []string{toolsetID},
		"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, idem("workflow-capture-final-only"), http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+finalOnlyDraft["id"].(string)+"/activate", nil, withMatch(finalOnlyDraft, idem("workflow-capture-final-only-activate")), http.StatusOK)
	finalOnlyKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Workflow final-only capture", "project_id": project, "scopes": []string{"inference", "tools"}, "allowed_routes": []string{finalOnlySlug}, "allowed_toolsets": []string{toolsetID}}, idem("workflow-capture-final-only-key"), http.StatusCreated)
	h.want(owner, http.MethodPatch, "/api/v1/observability/capture", map[string]any{"enabled": true}, etagHeader(h.want(owner, http.MethodGet, "/api/v1/observability/capture", nil, nil, http.StatusOK)), http.StatusOK)
	receiver, exportServer := newExportReceiver(t, "", http.StatusNoContent)
	captureSink := h.want(owner, http.MethodPost, "/api/v1/observability/sinks", map[string]any{"name": "workflow-capture", "type": "https", "destination": exportServer.URL, "streams": []string{"requests"}, "format": "json"}, idem(uuid.NewString()), http.StatusCreated)
	for _, captureRoute := range []string{controlSlug, projectedSlug, finalOnlySlug} {
		h.want(owner, http.MethodPost, "/api/v1/observability/capture-policies", map[string]any{"route_slug": captureRoute, "sink": captureSink["export_sink_id"], "sample_ratio": "1", "include": []string{"input", "output", "tool_calls"}, "max_bytes": 65536}, idem(uuid.NewString()), http.StatusCreated)
	}
	manager := &export.Manager{Pool: h.Pool, Keys: h.Ring2(), Installation: h.Server.Installation, Sender: export.NewSender(loopbackPolicy()), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h.Gateway.Capture = manager
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		return err
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { manager.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(3 * time.Second)
	for _, captureRoute := range []string{controlSlug, projectedSlug, finalOnlySlug} {
		for {
			collector := manager.Resolve(project, captureRoute, "", "", uuid.NewString())
			if collector != nil {
				collector.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("capture policy for %s did not compile", captureRoute)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	h.refresh()
	controlStatus, controlResponse, _ := h.gateway(http.MethodPost, "/v1/chat/completions", mcpExecString(t, controlKey, "secret"), map[string]any{"model": controlSlug, "messages": []any{map[string]any{"role": "user", "content": "capture-control"}}})
	if controlStatus != http.StatusOK {
		t.Fatalf("capture control: status=%d response=%v", controlStatus, controlResponse)
	}
	controlExports := awaitEnvelope(t, receiver, 1)
	controlData, err := json.Marshal(controlExports[0]["data"])
	if err != nil || !strings.Contains(string(controlData), "capture-control") {
		t.Fatalf("capture control not exported: %s %v", controlData, err)
	}
	responsesWorkflow.Store(true)
	responsesStatus, responsesBody, headers := h.gateway(http.MethodPost, "/v1/responses", mcpExecString(t, projectedKey, "secret"), map[string]any{"model": projectedSlug, "input": prompt, "max_output_tokens": 100})
	if responsesStatus != http.StatusOK || !strings.Contains(fmt.Sprint(responsesBody), argument) || !strings.Contains(fmt.Sprint(responsesBody), answer) {
		actualProvider := h.Runtime.Release().Snapshot.Providers[projectedProvider["id"].(string)]
		t.Fatalf("projected Responses workflow: status=%d response=%v profile=%s capabilities=%v", responsesStatus, responsesBody, actualProvider.ProfileID, actualProvider.Capabilities)
	}
	var captured bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT payload_captured FROM olp.requests WHERE id=$1`, headers.Get("X-Request-Id")).Scan(&captured); err != nil {
		t.Fatal(err)
	}
	if captured {
		t.Fatal("Responses workflow unexpectedly captured")
	}
	finalOnlyStatus, finalOnlyBody, finalOnlyHeaders := h.gateway(http.MethodPost, "/v1/responses", mcpExecString(t, finalOnlyKey, "secret"), map[string]any{"model": finalOnlySlug, "input": prompt, "max_output_tokens": 100})
	if finalOnlyStatus != http.StatusOK || strings.Contains(fmt.Sprint(finalOnlyBody), argument) || strings.Contains(fmt.Sprint(finalOnlyBody), answer) {
		t.Fatalf("final-only Responses workflow: status=%d response=%v", finalOnlyStatus, finalOnlyBody)
	}
	var finalOnlyCaptured bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT payload_captured FROM olp.requests WHERE id=$1`, finalOnlyHeaders.Get("X-Request-Id")).Scan(&finalOnlyCaptured); err != nil {
		t.Fatal(err)
	}
	if finalOnlyCaptured {
		t.Fatal("final-only Responses workflow unexpectedly captured")
	}
	for _, envelope := range receiver.received() {
		data, err := json.Marshal(envelope["data"])
		if err != nil || strings.Contains(string(data), prompt) || strings.Contains(string(data), argument) || strings.Contains(string(data), answer) {
			t.Fatalf("Responses workflow content reached capture export: %s %v", data, err)
		}
	}
	for _, table := range []string{"agent_runs", "agent_steps", "execution_debits", "mcp_tool_calls", "attempt_usage_facts", "requests", "attempts", "export_records"} {
		var leaks int
		query := "SELECT count(*) FROM olp." + table + " AS row WHERE row_to_json(row)::text LIKE '%' || $1 || '%' OR row_to_json(row)::text LIKE '%' || $2 || '%' OR row_to_json(row)::text LIKE '%' || $3 || '%'"
		if err := h.Pool.QueryRow(t.Context(), query, prompt, argument, answer).Scan(&leaks); err != nil || leaks != 0 {
			t.Fatalf("Responses workflow content reached %s: count=%d error=%v", table, leaks, err)
		}
	}
	concurrentKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Workflow concurrency", "project_id": project, "scopes": []string{"inference", "tools"}, "allowed_routes": []string{slug}, "allowed_toolsets": []string{toolsetID}, "run_limits": map[string]any{"max_concurrent_runs": 1}}, idem("workflow-concurrency-key"), http.StatusCreated)
	h.refresh()
	concurrentSecret := mcpExecString(t, concurrentKey, "secret")
	request := map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "concurrency"}}, "max_tokens": 100, "olp_toolsets": []any{}}
	blockRun.Store(true)
	first := make(chan int, 1)
	go func() {
		status, _, _ := h.gateway(http.MethodPost, "/v1/chat/completions", concurrentSecret, request)
		first <- status
	}()
	select {
	case <-runStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("first workflow did not reach the provider")
	}
	second := make(chan int, 1)
	go func() {
		status, _, _ := h.gateway(http.MethodPost, "/v1/chat/completions", concurrentSecret, request)
		second <- status
	}()
	select {
	case status := <-second:
		if status != http.StatusTooManyRequests {
			t.Fatalf("concurrent workflow returned %d, want 429", status)
		}
	case <-runStarted:
		t.Fatal("concurrent workflow reached the provider")
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent workflow did not refuse promptly")
	}
	blockRun.Store(false)
	runRelease <- struct{}{}
	select {
	case status := <-first:
		if status != http.StatusOK {
			t.Fatalf("first workflow returned %d", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first workflow did not finish")
	}
	status, result, _ := h.gateway(http.MethodPost, "/v1/chat/completions", concurrentSecret, request)
	if status != http.StatusOK {
		t.Fatalf("completed workflow did not release its slot: status=%d result=%v", status, result)
	}
	blockRun.Store(true)
	unlimitedRun := make(chan int, 1)
	go func() {
		status, _, _ := h.gateway(http.MethodPost, "/v1/chat/completions", secret, request)
		unlimitedRun <- status
	}()
	select {
	case <-runStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("unlimited workflow did not reach the provider")
	}
	limitedRequest := map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "limited concurrency"}}, "max_tokens": 100, "olp_toolsets": []any{}, "olp_run_limits": map[string]any{"max_concurrent_runs": 1}}
	limitedRun := make(chan int, 1)
	go func() {
		status, _, _ := h.gateway(http.MethodPost, "/v1/chat/completions", secret, limitedRequest)
		limitedRun <- status
	}()
	select {
	case limitedStatus := <-limitedRun:
		if limitedStatus != http.StatusTooManyRequests {
			t.Fatalf("limited workflow returned %d while an unlimited workflow was active", limitedStatus)
		}
	case <-runStarted:
		t.Fatal("limited workflow reached provider while an unlimited workflow was active")
	case <-time.After(10 * time.Second):
		t.Fatal("limited workflow did not refuse promptly")
	}
	blockRun.Store(false)
	runRelease <- struct{}{}
	select {
	case unlimitedStatus := <-unlimitedRun:
		if unlimitedStatus != http.StatusOK {
			t.Fatalf("unlimited workflow returned %d", unlimitedStatus)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("unlimited workflow did not finish")
	}
	responsesWorkflow.Store(true)
	beforeModels, beforeTools := modelCalls.Load(), toolCalls.Load()
	costStatus, costResult, _ := h.gateway(http.MethodPost, "/v1/chat/completions", secret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "cost bounded"}}, "max_tokens": 100, "olp_run_limits": map[string]any{"max_run_cost": "1105"}})
	if costStatus != http.StatusBadRequest || !strings.Contains(fmt.Sprint(costResult), "run_cost_limit") || modelCalls.Load()-beforeModels != 1 || toolCalls.Load()-beforeTools != 1 {
		t.Fatalf("workflow cost bound: status=%d response=%v model calls=%d tool calls=%d", costStatus, costResult, modelCalls.Load()-beforeModels, toolCalls.Load()-beforeTools)
	}
	boundedKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Scoped workflow limits", "project_id": project, "scopes": []string{"inference", "tools"}, "allowed_routes": []string{slug}, "allowed_toolsets": []string{toolsetID}, "run_limits": map[string]any{"max_tool_calls": 2, "max_duration_seconds": 4, "max_concurrent_runs": 2}}, idem("scoped-workflow-key"), http.StatusCreated)
	h.refresh()
	boundedSecret := mcpExecString(t, boundedKey, "secret")
	beforeScopedTools := toolCalls.Load()
	limitedStatus, limitedResult, _ := h.gateway(http.MethodPost, "/v1/chat/completions", boundedSecret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "scoped tool limit"}}, "max_tokens": 100, "olp_run_limits": map[string]any{"max_tool_calls": 0}})
	if limitedStatus != http.StatusBadRequest || !strings.Contains(fmt.Sprint(limitedResult), "tool_call_limit") || toolCalls.Load() != beforeScopedTools {
		t.Fatalf("scoped tool limit: status=%d response=%v tool calls=%d", limitedStatus, limitedResult, toolCalls.Load()-beforeScopedTools)
	}
	deadlineModel.Store(true)
	defer deadlineModel.Store(false)
	firstScoped := make(chan int, 1)
	go func() {
		status, _, _ := h.gateway(http.MethodPost, "/v1/chat/completions", boundedSecret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "scoped concurrency"}}, "max_tokens": 100})
		firstScoped <- status
	}()
	select {
	case <-deadlineStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("first scoped workflow did not reach the blocked model")
	}
	beforeScopedModels := modelCalls.Load()
	scopedStatus, scopedResult, _ := h.gateway(http.MethodPost, "/v1/chat/completions", boundedSecret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "scoped concurrency"}}, "max_tokens": 100, "olp_run_limits": map[string]any{"max_concurrent_runs": 1}})
	if scopedStatus != http.StatusTooManyRequests || modelCalls.Load() != beforeScopedModels {
		t.Fatalf("scoped concurrency limit: status=%d response=%v model calls=%d", scopedStatus, scopedResult, modelCalls.Load()-beforeScopedModels)
	}
	select {
	case <-deadlineCanceled:
	case <-time.After(6 * time.Second):
		t.Fatal("key deadline did not cancel the blocked workflow")
	}
	select {
	case status := <-firstScoped:
		if status == http.StatusOK {
			t.Fatal("key deadline returned success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first scoped workflow did not finish")
	}
	deadlineModel.Store(false)
	for _, test := range []struct {
		name  string
		block *atomic.Bool
	}{
		{name: "model", block: &deadlineModel},
		{name: "tool", block: &deadlineTool},
	} {
		t.Run(test.name+" deadline", func(t *testing.T) {
			test.block.Store(true)
			defer test.block.Store(false)
			result := make(chan int, 1)
			go func() {
				status, _, _ := h.gateway(http.MethodPost, "/v1/chat/completions", boundedSecret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "deadline"}}, "max_tokens": 100, "olp_run_limits": map[string]any{"max_duration_seconds": 1}})
				result <- status
			}()
			select {
			case <-deadlineStarted:
			case <-time.After(10 * time.Second):
				t.Fatal("workflow did not reach the blocked upstream")
			}
			select {
			case <-deadlineCanceled:
			case <-time.After(2500 * time.Millisecond):
				t.Fatal("caller deadline did not cancel the blocked upstream")
			}
			select {
			case status := <-result:
				if status == http.StatusOK {
					t.Fatal("deadline returned success")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("workflow did not finish after deadline cancellation")
			}
		})
	}
}

func TestGatewayToolWorkflowWithPinnedGeminiCache(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Pinned Gemini workflow")
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := func() any {
			switch request.Method {
			case "initialize":
				if request.Params.ProtocolVersion == "" {
					return nil
				}
				w.Header().Set("Mcp-Session-Id", "pinned-gemini-session")
				return map[string]any{"protocolVersion": request.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}
			case "tools/list":
				return map[string]any{"tools": []any{map[string]any{"name": "lookup", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}
			case "tools/call":
				return map[string]any{"content": []any{map[string]any{"type": "text", "text": "pinned-result"}}, "isError": false}
			default:
				return nil
			}
		}()
		if result == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer toolServer.Close()
	registered := h.want(owner, "POST", "/api/v1/mcp-servers", map[string]any{"name": "Pinned tools", "project_id": project, "transport": "streamable_http", "endpoint": toolServer.URL, "enabled": true, "unit_price": "0.25"}, idem("pinned-server"), http.StatusCreated)
	serverID := mcpExecString(t, registered, "id")
	tool := agentcontract.Tool{ServerID: serverID, RevisionID: mcpExecString(t, registered, "revision_id"), CatalogDigest: mcpExecString(t, mcpExecObject(t, registered, "catalog"), "digest"), Name: "lookup", Alias: agentcontract.ToolAlias(serverID, "lookup"), Classification: "read_only"}
	toolset := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{"project_id": project, "name": "Pinned toolset", "toolset": agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}, idem("pinned-toolset"), http.StatusCreated)
	toolsetID := mcpExecString(t, toolset, "id")
	h.want(owner, "POST", "/api/v1/toolsets/"+toolsetID+"/publish", nil, withMatch(toolset, idem("pinned-publish")), http.StatusOK)

	var calls atomic.Int64
	var refuseTools atomic.Bool
	var uncertifiedDispatches atomic.Int64
	providerRequests := make(chan map[string]json.RawMessage, 2)
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]string{"name": "models/" + vendorModel}}})
			return
		}
		var request map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if refuseTools.Load() && !strings.Contains(string(request["tools"]), "olp_certification_probe") {
			uncertifiedDispatches.Add(1)
		}
		name := func() string {
			if strings.Contains(string(request["tools"]), "olp_certification_probe") {
				if refuseTools.Load() {
					return ""
				}
				return "olp_certification_probe"
			}
			if len(request["cachedContent"]) == 0 {
				return ""
			}
			providerRequests <- request
			calls.Add(1)
			if strings.Contains(string(request["contents"]), "functionResponse") {
				return ""
			}
			return tool.Alias
		}()
		parts := func() []any {
			if name != "" {
				return []any{map[string]any{"functionCall": map[string]any{"name": name, "args": map[string]any{}}}}
			}
			return []any{map[string]any{"text": "pinned-final"}}
		}()
		_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "finishReason": "STOP"}}, "usageMetadata": map[string]any{"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10}})
	}))
	defer vendor.Close()
	configuration := map[string]any{"kind": "gemini", "profile_id": "gemini-generation", "profile_revision": "1", "endpoint": vendor.URL + "/v1beta", "auth_mode": "api_key"}
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Pinned Gemini", "project_id": project, "configuration": configuration, "model": vendorModel, "credential": vendorSecret}, idem("pinned-provider"), http.StatusCreated)
	providerID := mcpExecString(t, provider, "id")
	providerPath := "/api/v1/providers/" + providerID
	probe := h.want(owner, "POST", providerPath+"/probe", nil, etagHeader(provider), http.StatusOK)
	if probe["succeeded"] != true {
		t.Fatalf("provider probe failed: %v", probe)
	}
	modelResponse := h.want(owner, "GET", providerPath+"/models", nil, nil, http.StatusOK)
	modelItems, hasModels := modelResponse["items"].([]any)
	if !hasModels || len(modelItems) != 1 {
		t.Fatalf("unexpected provider models: %v", modelResponse)
	}
	model, isModel := modelItems[0].(map[string]any)
	if !isModel {
		t.Fatalf("unexpected provider model: %v", modelItems[0])
	}
	modelID := mcpExecString(t, model, "id")
	declared := h.want(owner, "PATCH", providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "generation", "surface": "gemini", "mode": "unary"}}}, etagHeader(provider), http.StatusOK)
	certified := h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(declared), http.StatusOK)
	results, ok := certified["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("provider certification failed: %v", certified)
	}
	result, isResult := results[0].(map[string]any)
	if !isResult || result["tool_calls"] != true {
		t.Fatalf("provider certification failed: %v", certified)
	}
	current := h.want(owner, "GET", providerPath, nil, nil, http.StatusOK)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(current, idem("pinned-provider-activate")), http.StatusOK)
	priceEffective := time.Now().Add(-time.Second)
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{
		"effective_at": priceEffective.UTC().Format(time.RFC3339Nano),
		"prices":       []any{map[string]any{"provider_kind": "gemini", "provider_id": providerID, "model": vendorModel, "operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000"}},
	}, idem("pinned-price"), http.StatusCreated)
	eventually(t, "Gemini price in routing inputs", 30*time.Second, func() bool {
		h.refresh()
		inputs := h.Runtime.RoutingInputs()
		return inputs != nil && inputs.Price("gemini", providerID, "", vendorModel, "generation", time.Now()) != nil
	})
	const slug = "pinned-gemini-workflow"
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": project, "slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "tool_execution": "gateway", "toolsets": []string{toolsetID}, "targets": []any{map[string]any{"provider_id": providerID, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("pinned-route"), http.StatusCreated)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("pinned-route-activate")), http.StatusOK)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Pinned caller", "project_id": project, "scopes": []string{"inference", "tools"}, "allowed_routes": []string{slug}, "allowed_toolsets": []string{toolsetID}, "allow_provider_state": true}, idem("pinned-key"), http.StatusCreated)
	prompt := h.want(owner, "POST", "/api/v1/prompts", map[string]any{"project_id": project, "name": "Pinned prompt", "prompt": json.RawMessage(`{"messages":[{"role":"user","content":"Hello {{name}}"}],"variable_schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]},"default_parameters":{"max_output_tokens":100}}`)}, idem("pinned-prompt"), http.StatusCreated)
	promptID := mcpExecString(t, prompt, "id")
	published := h.want(owner, "POST", "/api/v1/prompts/"+promptID+"/publish", nil, withMatch(prompt, idem("pinned-prompt-publish")), http.StatusOK)
	promptRevision := mcpExecString(t, published, "revision_id")
	secret := mcpExecString(t, key, "secret")
	h.refresh()
	authority, err := h.Runtime.Authenticate(secret)
	if err != nil {
		t.Fatal(err)
	}
	route := h.Runtime.Release().Snapshot.Routes[slug]
	pinnedProvider := h.Runtime.Release().Snapshot.Providers[providerID]
	expires := time.Now().Add(time.Hour)
	version := retainedapi.Revision
	cache, err := h.Gateway.Resources.Put(t.Context(), &resources.Resource{Kind: "context_cache", APIKeyID: authority.ID, RouteSlug: slug, ProviderID: pinnedProvider.ID, ProviderRevisionID: pinnedProvider.RevisionID, RouteRevisionID: route.RevisionID, TargetID: route.Targets[0].ID, SlotID: pinnedProvider.Slots[0].ID, CredentialID: pinnedProvider.Slots[0].CredentialID, UpstreamID: "cachedContents/pinned-test", State: "active", Metadata: json.RawMessage(`{"model":"fixture-model","upstream_model":"fixture-model","total_token_count":4,"no_cached_tools":true}`), ExpiresAt: &expires, ContractVersion: &version})
	if err != nil {
		t.Fatal(err)
	}
	valkey, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(valkey, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	limiter.UseDurableDebits(h.Pool)
	store := &limits.DebitStore{Pool: h.Pool, Limiter: limiter}
	h.Gateway.Admission = gateway.NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
	h.Gateway.Debits = gateway.DurableSessionDebits(store)
	h.Gateway.WorkflowRuns = &agents.WorkflowStore{Pool: h.Pool, Auth: h.AuthKey, Debits: store}
	h.Gateway.WorkflowTools = &mcp.Server{Runtime: h.Runtime, Egress: &h.Policy, Auth: h.AuthKey, Limiter: limiter, Ledger: &mcp.DurableLedger{Pool: h.Pool, Store: store, Auth: h.AuthKey}}
	h.Gateway.Sink = &gateway.AccountingSink{Emitter: usage.NewEmitter(8)}
	response, raw := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/models/"+slug+":generateContent", secret, []byte(fmt.Sprintf(`{"cachedContent":%q,"olp_prompt":{"id":%q,"revision_id":%q,"variables":{"name":"world"}}}`, cache.ID, promptID, promptRevision)))
	if response.StatusCode != http.StatusOK || calls.Load() != 2 || !strings.Contains(string(raw), "pinned-final") {
		t.Fatalf("pinned workflow failed: status=%d body=%s model calls=%d", response.StatusCode, raw, calls.Load())
	}
	first, second := <-providerRequests, <-providerRequests
	if string(first["cachedContent"]) != `"cachedContents/pinned-test"` || string(second["cachedContent"]) != `"cachedContents/pinned-test"` || !strings.Contains(string(first["contents"]), "Hello world") || !strings.Contains(string(second["contents"]), "pinned-result") || len(first["olp_prompt"]) != 0 {
		t.Fatalf("workflow lost its pinned cache or tool result: first=%s second=%s", first["cachedContent"], second["cachedContent"])
	}
	var models, tools, runs int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE kind='model' AND state='settled'),count(*) FILTER(WHERE kind='tool' AND state='settled'),count(DISTINCT run_id) FROM olp.execution_debits WHERE api_key_id=$1`, authority.ID).Scan(&models, &tools, &runs); err != nil {
		t.Fatal(err)
	}
	if models != 2 || tools != 1 || runs != 1 {
		t.Fatalf("pinned workflow did not settle all steps: models=%d tools=%d runs=%d", models, tools, runs)
	}
	refuseTools.Store(true)
	currentProvider := h.want(owner, "GET", providerPath, nil, nil, http.StatusOK)
	uncertified := h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(currentProvider), http.StatusOK)
	uncertifiedResults, hasResults := uncertified["results"].([]any)
	if !hasResults || len(uncertifiedResults) != 1 {
		t.Fatalf("unexpected recertification result: %v", uncertified)
	}
	uncertifiedResult, validResult := uncertifiedResults[0].(map[string]any)
	if !validResult || uncertifiedResult["succeeded"] != true || uncertifiedResult["tool_calls"] != false {
		t.Fatalf("provider without tool calls was incorrectly certified: %v", uncertified)
	}
	recertifiedProvider := h.want(owner, "GET", providerPath, nil, nil, http.StatusOK)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(recertifiedProvider, idem("pinned-uncertified-activate")), http.StatusOK)
	uncertifiedDispatches.Store(0)
	h.refresh()
	refusal, refusalBody := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/models/"+slug+":generateContent", secret, []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	if refusal.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(refusalBody), "upstream_unavailable") || uncertifiedDispatches.Load() != 0 {
		t.Fatalf("uncertified provider was not refused before dispatch: status=%d body=%s calls=%d", refusal.StatusCode, refusalBody, uncertifiedDispatches.Load())
	}
}

func TestAnthropicToolCallsCertification(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Anthropic tool certification")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		var request map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		content := func() []any {
			if strings.Contains(string(request["tools"]), "olp_certification_probe") {
				return []any{map[string]any{"type": "tool_use", "id": "probe-call", "name": "olp_certification_probe", "input": map[string]any{}}}
			}
			return []any{map[string]any{"type": "text", "text": "ready"}}
		}()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "probe-message", "type": "message", "role": "assistant", "model": vendorModel, "content": content, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
	}))
	defer upstream.Close()
	configuration := map[string]any{"kind": "anthropic", "profile_id": "anthropic-messages", "profile_revision": "1", "endpoint": upstream.URL + "/v1", "auth_mode": "api_key"}
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Anthropic tools", "project_id": project, "configuration": configuration, "model": vendorModel, "credential": vendorSecret}, idem("anthropic-tool-provider"), http.StatusCreated)
	providerPath := "/api/v1/providers/" + mcpExecString(t, provider, "id")
	modelResponse := h.want(owner, "GET", providerPath+"/models", nil, nil, http.StatusOK)
	models, hasModels := modelResponse["items"].([]any)
	if !hasModels || len(models) != 1 {
		t.Fatalf("unexpected provider models: %v", modelResponse)
	}
	model, isModel := models[0].(map[string]any)
	if !isModel {
		t.Fatalf("unexpected provider model: %v", models[0])
	}
	modelID := mcpExecString(t, model, "id")
	declared := h.want(owner, "PATCH", providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "generation", "surface": "anthropic", "mode": "unary"}}}, etagHeader(provider), http.StatusOK)
	certification := h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(declared), http.StatusOK)
	results, ok := certification["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("provider certification failed: %v", certification)
	}
	result, isResult := results[0].(map[string]any)
	if !isResult || result["tool_calls"] != true {
		t.Fatalf("provider tool-call certification failed: %v", certification)
	}
}

func TestNativeStreamingToolCallsCertification(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Native streaming tool certification")
	for _, tc := range []struct {
		surface, profile, basePath string
		supportsTools              bool
	}{
		{"anthropic", "anthropic-messages", "/v1", true},
		{"anthropic", "anthropic-messages", "/v1", false},
		{"gemini", "gemini-generation", "/v1beta", true},
		{"gemini", "gemini-generation", "/v1beta", false},
		{"openai", "openai-responses", "/v1", true},
		{"openai", "openai-responses", "/v1", false},
	} {
		t.Run(fmt.Sprintf("%s/tools=%t", tc.surface, tc.supportsTools), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if tc.surface == "gemini" {
						_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]string{"name": "models/" + vendorModel}}})
					} else {
						_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
					}
					return
				}
				var request struct {
					Tools  []json.RawMessage `json:"tools"`
					Stream bool              `json:"stream"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				stream := request.Stream || strings.HasSuffix(r.URL.Path, ":streamGenerateContent")
				if tc.surface == "openai" {
					item := func() map[string]any {
						if len(request.Tools) > 0 && tc.supportsTools {
							return map[string]any{"id": "function-probe", "type": "function_call", "status": "completed", "call_id": "call-probe", "name": "olp_certification_probe", "arguments": "{}"}
						}
						return map[string]any{"id": "message", "type": "message", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ready", "annotations": []any{}}}}
					}()
					encodedItem, _ := json.Marshal(item)
					response, _ := json.Marshal(map[string]any{"id": "response", "object": "response", "status": "completed", "model": vendorModel, "output": []any{item}, "usage": map[string]int{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}})
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":0,\"output_index\":0,\"item\":%s}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":%s}\n\n", encodedItem, response)
					return
				}
				if len(request.Tools) == 0 || !tc.supportsTools {
					kindGeneration(w, tc.surface, stream)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.surface == "gemini" {
					fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"olp_certification_probe\",\"args\":{}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":2,\"totalTokenCount\":5}}\n\n")
					return
				}
				fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"certification\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"usage\":{\"input_tokens\":3}}}\n\n", vendorModel)
				fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"probe-call\",\"name\":\"olp_certification_probe\",\"input\":{}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer upstream.Close()
			name := fmt.Sprintf("%s streaming tools %t", tc.surface, tc.supportsTools)
			configuration := map[string]any{"kind": tc.surface, "profile_id": tc.profile, "profile_revision": "1", "endpoint": upstream.URL + tc.basePath, "auth_mode": "api_key"}
			provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": name, "project_id": project, "configuration": configuration, "model": vendorModel, "credential": vendorSecret}, idem("streaming-provider-"+name), http.StatusCreated)
			path := "/api/v1/providers/" + mcpExecString(t, provider, "id")
			models := h.want(owner, "GET", path+"/models", nil, nil, http.StatusOK)
			items, itemsOK := models["items"].([]any)
			if !itemsOK || len(items) != 1 {
				t.Fatalf("unexpected provider models: %v", models)
			}
			model, modelOK := items[0].(map[string]any)
			if !modelOK {
				t.Fatalf("unexpected provider model: %v", items[0])
			}
			modelPath := path + "/models/" + mcpExecString(t, model, "id")
			declared := h.want(owner, "PATCH", modelPath, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "generation", "surface": tc.surface, "mode": "streaming"}}}, etagHeader(provider), http.StatusOK)
			certification := h.want(owner, "POST", modelPath+"/certify", nil, etagHeader(declared), http.StatusOK)
			results, resultsOK := certification["results"].([]any)
			if !resultsOK || len(results) != 1 {
				t.Fatalf("unexpected certification results: %v", certification)
			}
			result, resultOK := results[0].(map[string]any)
			if !resultOK || result["succeeded"] != true || result["tool_calls"] != tc.supportsTools {
				t.Fatalf("streaming tool capability was not certified correctly: %v", certification)
			}
		})
	}
}
