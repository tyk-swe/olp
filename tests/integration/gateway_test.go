//go:build integration

package integration_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	mu      sync.Mutex
	secrets map[string]bool
	fail    atomic.Bool
	delay   atomic.Int64
	chats   atomic.Int64
	models  atomic.Int64
	cancels atomic.Int64
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

// TestCapabilitiesReportConfiguredEnforcement proves the two capability flags
// the console gates its enforced branches on report what this installation is
// composed with instead of asserting enforcement. Without shared state
// admission has no backend and the worker plane that applies retention and
// aggregation never starts, so both are false; an installation configured with
// it reports both as true. Neither flag claims a worker replica is alive: a
// control process cannot observe one.
func TestCapabilitiesReportConfiguredEnforcement(t *testing.T) {
	unconfigured := newAccessHarness(t)
	caps := unconfigured.want(nil, "GET", "/api/v3/auth/capabilities", nil, nil, 200)
	if caps["limits_enforced"] != false || caps["retention_enforced"] != false {
		t.Fatalf("capabilities without shared state: %v", caps)
	}
	// Both flags are decided during composition, before anything is served,
	// which is what setting them on a fresh harness stands in for here.
	configured := newAccessHarness(t)
	configured.Server.LimitsEnforced, configured.Server.RetentionEnforced = true, true
	caps = configured.want(nil, "GET", "/api/v3/auth/capabilities", nil, nil, 200)
	if caps["limits_enforced"] != true || caps["retention_enforced"] != true {
		t.Fatalf("capabilities with shared state: %v", caps)
	}
}

func TestGatewayFromEmptyInstallationToSDKTraffic(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	// This harness composes the surfaces without the shared state that
	// admission and the worker plane both need, so neither stored limits nor
	// retention are reported as enforced here.
	caps := h.want(nil, "GET", "/api/v3/auth/capabilities", nil, nil, 200)
	if caps["gateway_available"] != true || caps["limits_enforced"] != false || caps["retention_enforced"] != false {
		t.Fatalf("capabilities %v", caps)
	}
	if h.want(owner, "GET", "/api/v3/provider-kinds/openai_compatible/capabilities", nil, nil, 200)["provider_kind"] != "openai_compatible" {
		t.Fatal("kind capabilities")
	}
	h.want(owner, "GET", "/api/v3/provider-kinds/bedrock/capabilities", nil, nil, 200)

	// Unsafe egress is rejected at configuration time without any dispatch.
	configuration := func(endpoint string) map[string]any {
		return map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": endpoint}
	}
	unsafe := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "Metadata", "configuration": configuration("http://169.254.169.254/latest"), "credential": vendorSecret}, map[string]string{"Idempotency-Key": "unsafe"}, 422)
	if problemCode(t, unsafe) != "validation_failed" {
		t.Fatalf("unsafe endpoint: %v", unsafe)
	}
	h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "Unsupported", "configuration": map[string]any{"kind": "unsupported", "auth_mode": "api_key"}, "credential": "x"}, map[string]string{"Idempotency-Key": "unsupported"}, 422)

	// Certification and inference must agree on a base URL with a trailing slash.
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "Fixture vendor", "configuration": configuration(up.URL + "/v1/"), "credential": vendorSecret}, map[string]string{"Idempotency-Key": "provider"}, 201)
	validateManagementResponse(t, "POST", "/api/v3/providers", 201, created)
	pid := created["id"].(string)
	providerPath := "/api/v3/providers/" + pid
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
	draftInput := map[string]any{"slug": routeSlug, "overall_timeout_ms": 5000, "max_attempts": 2, "targets": []any{target}}
	unknown := h.want(owner, "POST", "/api/v3/route-drafts", map[string]any{"slug": "bad", "overall_timeout_ms": 5000, "max_attempts": 2, "targets": []any{map[string]any{"provider_id": pid, "provider_model": "missing", "priority": 0, "weight": 1, "timeout_ms": 2000}}}, map[string]string{"Idempotency-Key": "draft-unknown"}, 422)
	if problemCode(t, unknown) != "validation_failed" {
		t.Fatalf("unknown model %v", unknown)
	}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", draftInput, map[string]string{"Idempotency-Key": "draft"}, 201)
	validateManagementResponse(t, "POST", "/api/v3/route-drafts", 201, draft)
	draftPath := "/api/v3/route-drafts/" + draft["id"].(string)
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
	routes := h.want(owner, "GET", "/api/v3/routes", nil, nil, 200)
	validateManagementResponse(t, "GET", "/api/v3/routes", 200, routes)
	generations := h.want(owner, "GET", "/api/v3/runtime-generations", nil, nil, 200)
	validateManagementResponse(t, "GET", "/api/v3/runtime-generations", 200, generations)
	if len(generations["items"].([]any)) != 2 {
		t.Fatalf("generations %v", generations)
	}

	// Keys: explicit scopes and allowlists gate model listing and inference.
	fullKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "sdk", "scopes": []string{"inference", "models_read"}, "allowed_routes": []string{routeSlug}}, map[string]string{"Idempotency-Key": "key-full"}, 201)
	otherKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "other", "scopes": []string{"inference", "models_read"}, "allowed_routes": []string{"another-route"}}, map[string]string{"Idempotency-Key": "key-other"}, 201)
	readKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "read", "scopes": []string{"models_read"}}, map[string]string{"Idempotency-Key": "key-read"}, 201)
	unrestrictedKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "unrestricted", "scopes": []string{"inference", "models_read"}, "allowed_routes": []string{}}, map[string]string{"Idempotency-Key": "key-unrestricted"}, 201)
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
		decisions := h.list(owner, "POST", "/api/v3/routing/simulate", map[string]any{"operation": map[string]any{"operation": "generation", "request": map[string]any{"route": routeSlug}}, "surface": "openai", "mode": "unary", "seed": "tenant-a", "api_key_id": tc.key["id"]}, nil, 200)
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
	h.want(owner, "POST", "/api/v3/playground", map[string]any{"model": routeSlug, "input": "hi", "routing": map[string]any{"strategy": "unknown"}}, nil, 422)
	h.want(owner, "POST", "/api/v3/playground", map[string]any{"model": routeSlug, "input": "hi", "routing": map[string]any{"only": []string{"vendor"}}}, nil, 422)
	play := h.want(owner, "POST", "/api/v3/playground", map[string]any{"model": routeSlug, "input": "hi", "routing": map[string]any{"strategy": "weighted", "allow_fallbacks": false}}, nil, 200)
	validateManagementResponse(t, "POST", "/api/v3/playground", 200, play)
	if play["output_text"] != vendorAnswer || play["model"] != routeSlug || len(play["routing"].([]any)) != 1 {
		t.Fatalf("playground %v", play)
	}
	health := h.want(owner, "GET", "/api/v3/provider-health", nil, nil, 200)
	validateManagementResponse(t, "GET", "/api/v3/provider-health", 200, health)
	if items := health["items"].([]any); len(items) != 1 || items[0].(map[string]any)["attempt_count"].(float64) < 3 {
		t.Fatalf("health %v", health)
	}

	// Draft edits never change serving traffic until activation.
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	edited := h.want(owner, "PATCH", providerPath, map[string]any{"name": "Fixture vendor (edited)", "configuration": configuration(up.URL + "/elsewhere/v1")}, etagHeader(detail), 200)
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
	keyPath := "/api/v3/api-keys/" + fullKey["id"].(string)
	keyRecord := h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "POST", keyPath+"/revoke", nil, withMatch(keyRecord, map[string]string{"Idempotency-Key": "key-revoke"}), 200)
	if status, _, _ := h.gateway("POST", "/v1/chat/completions", secret, chat); status != 200 {
		t.Fatalf("pinned authority must serve until refresh: %d", status)
	}
	h.refresh()
	if status, body, _ := h.gateway("POST", "/v1/chat/completions", secret, chat); status != 401 || h.gatewayCode(status, body) != "invalid_api_key" {
		t.Fatalf("revoked key %d %v", status, body)
	}
	replacement := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "sdk-2", "scopes": []string{"inference", "models_read"}}, map[string]string{"Idempotency-Key": "key-2"}, 201)["secret"].(string)
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
	routeRevisions := h.want(owner, "GET", "/api/v3/routes/"+routeID+"/revisions", nil, nil, 200)
	validateManagementResponse(t, "GET", "/api/v3/routes/"+routeID+"/revisions", 200, routeRevisions)
	revisionID := routeRevisions["items"].([]any)[0].(map[string]any)["id"].(string)
	restoredDraft := h.want(owner, "POST", "/api/v3/routes/"+routeID+"/revisions/"+revisionID+"/restore-as-draft", nil, map[string]string{"Idempotency-Key": "route-restore"}, 201)
	validateManagementResponse(t, "POST", "/api/v3/routes/"+routeID+"/revisions/"+revisionID+"/restore-as-draft", 201, restoredDraft)
	if restoredDraft["based_on_revision_id"] != revisionID || restoredDraft["slug"] != routeSlug {
		t.Fatalf("restored draft %v", restoredDraft)
	}
	decisions := h.list(owner, "POST", "/api/v3/routing/simulate", map[string]any{"operation": map[string]any{"operation": "generation", "request": map[string]any{"route": routeSlug}}, "surface": "openai", "mode": "unary", "seed": "tenant-a"}, nil, 200)
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
