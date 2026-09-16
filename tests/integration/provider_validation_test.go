//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestActivationRequiresCurrentCredentialSlotValidation(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name": "Validated slots", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	providerID := created["id"].(string)
	path := "/api/v3/providers/" + providerID
	listPath := path + "/credential-slots"
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(created), 200)
	activate := func(key string, status int) {
		t.Helper()
		detail := h.want(owner, "GET", path, nil, nil, 200)
		result := h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": key}), status)
		if status == 422 && problemCode(t, result) != "slot_validation_required" {
			t.Fatalf("activation did not require slot validation: %v", result)
		}
	}
	activate("initial", 200)
	h.refresh()
	initial := h.Runtime.Release().Snapshot.Providers[providerID]
	slots := h.want(owner, "GET", listPath, nil, nil, 200)
	defaultID := slots["items"].([]any)[0].(map[string]any)["id"].(string)
	poolID := uuid.NewString()
	poolPath := listPath + "/" + poolID
	slots = h.want(owner, "PUT", poolPath, map[string]any{"slot": map[string]any{"name": "pool"}, "credential": "invalid"}, withMatch(slots, map[string]string{"Idempotency-Key": "add-invalid"}), 200)
	activate("reject-new", 422)
	failed := h.want(owner, "POST", poolPath+"/validate", nil, nil, 422)
	if problemCode(t, failed) != "slot_validation_failed" {
		t.Fatalf("invalid credential validated: %v", failed)
	}
	h.refresh()
	serving := h.Runtime.Release().Snapshot.Providers[providerID]
	if serving.RevisionID != initial.RevisionID || len(serving.Slots) != 1 {
		t.Fatalf("rejected activation changed the serving revision: %+v", serving)
	}
	slots = h.want(owner, "PUT", poolPath, map[string]any{"slot": map[string]any{"name": "pool", "enabled": false}}, withMatch(slots, map[string]string{"Idempotency-Key": "disable-invalid"}), 200)
	activate("disabled-slot", 200)
	up.accept(vendorRotated)
	slots = h.want(owner, "PUT", poolPath, map[string]any{"slot": map[string]any{"name": "pool"}, "credential": vendorRotated}, withMatch(slots, map[string]string{"Idempotency-Key": "replace-pool"}), 200)
	activate("reject-unvalidated", 422)
	calls := up.chats.Load()
	slots = h.want(owner, "POST", poolPath+"/validate", nil, nil, 200)
	if up.chats.Load() != calls+2 || slots["health"].(map[string]any)[poolID].(map[string]any)["validated_at"] == nil {
		t.Fatalf("validation must probe unary and streaming generation: %v", slots)
	}
	activate("validated-pool", 200)
	// Credential changes invalidate evidence even when the old model
	// certifications or a historical revision are still available.
	slots = h.want(owner, "PUT", listPath+"/"+defaultID, map[string]any{"slot": map[string]any{"name": "default"}, "credential": "invalid-default"}, withMatch(slots, map[string]string{"Idempotency-Key": "replace-default"}), 200)
	activate("reject-default", 422)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/restore-as-draft", nil, withMatch(detail, map[string]string{"Idempotency-Key": "restore"}), 200)
	activate("reject-restored", 422)
	up.accept("invalid-default")
	slots = h.want(owner, "POST", listPath+"/"+defaultID+"/validate", nil, nil, 200)
	activate("validated-default", 200)
	slots = h.want(owner, "PUT", poolPath, map[string]any{"slot": map[string]any{"name": "pool"}, "credential": "invalid-rotation"}, withMatch(slots, map[string]string{"Idempotency-Key": "rotate-pool"}), 200)
	if slots["health"].(map[string]any)[poolID].(map[string]any)["validated_at"] != nil {
		t.Fatal("rotation retained validation evidence")
	}
	activate("reject-rotated-pool", 422)
}

func TestSlotValidationProbesAllowedEnabledCapabilities(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	var denyStream atomic.Bool
	denyStream.Store(true)
	var mu sync.Mutex
	var probes []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"allowed"},{"id":"blocked"},{"id":"disabled"}]}`)
			return
		}
		var input struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		credential := r.Header.Get("Authorization")
		mu.Lock()
		probes = append(probes, fmt.Sprintf("%s/%s/%t", credential, input.Model, input.Stream))
		mu.Unlock()
		if credential == "Bearer list-only" || (credential == "Bearer limited" && (input.Model != "allowed" || (input.Stream && denyStream.Load()))) {
			http.Error(w, "generation denied", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/v1/responses" {
			writeResponsesFixture(w, input.Model, "OK", input.Stream)
			return
		}
		if input.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		} else {
			io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
		}
	}))
	t.Cleanup(up.Close)
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name": "Scoped access", "model": "allowed", "credential": "full",
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	path := "/api/v3/providers/" + created["id"].(string)
	detail := h.want(owner, "POST", path+"/discovery", map[string]any{"models": []any{}}, etagHeader(created), 200)
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelIDs := map[string]string{}
	for _, raw := range models["items"].([]any) {
		model := raw.(map[string]any)
		modelIDs[model["upstream_model"].(string)] = model["id"].(string)
	}
	for _, name := range []string{"blocked", "disabled"} {
		detail = h.want(owner, "PATCH", path+"/models/"+modelIDs[name], map[string]any{"enabled": name == "blocked", "capabilities": []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}}, etagHeader(detail), 200)
	}
	for _, name := range []string{"allowed", "blocked"} {
		h.want(owner, "POST", path+"/models/"+modelIDs[name]+"/certify", nil, etagHeader(detail), 200)
		detail = h.want(owner, "GET", path, nil, nil, 200)
	}
	listPath := path + "/credential-slots"
	slots := h.want(owner, "GET", listPath, nil, nil, 200)
	poolID := uuid.NewString()
	poolPath := listPath + "/" + poolID
	input := map[string]any{"slot": map[string]any{"name": "limited", "allowed_models": []string{"allowed", "disabled"}}, "credential": "limited"}
	slots = h.want(owner, "PUT", poolPath, input, withMatch(slots, map[string]string{"Idempotency-Key": "pool"}), 200)
	h.want(owner, "POST", poolPath+"/validate", nil, nil, 422)
	slots = h.want(owner, "GET", listPath, nil, nil, 200)
	if slots["health"].(map[string]any)[poolID].(map[string]any)["validated_at"] != nil {
		t.Fatal("unary-only access validated a streaming capability")
	}
	denyStream.Store(false)
	mu.Lock()
	probes = nil
	mu.Unlock()
	slots = h.want(owner, "POST", poolPath+"/validate", nil, nil, 200)
	mu.Lock()
	observed := append([]string(nil), probes...)
	mu.Unlock()
	if fmt.Sprint(observed) != "[Bearer limited/allowed/false Bearer limited/allowed/false Bearer limited/allowed/true Bearer limited/allowed/true]" {
		t.Fatalf("validation did not honor enabled models and restrictions: %v", observed)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "activate"}), 200)
	// Expanding access makes previously successful evidence stale.
	slots = h.want(owner, "PUT", poolPath, map[string]any{"slot": map[string]any{"name": "limited"}}, withMatch(slots, map[string]string{"Idempotency-Key": "expand"}), 200)
	if slots["health"].(map[string]any)[poolID].(map[string]any)["validated_at"] != nil {
		t.Fatal("expanded model access reused narrower evidence")
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "reject-expanded"}), 422)
	h.want(owner, "POST", poolPath+"/validate", nil, nil, 422)
	// Discovery permission alone cannot validate a slot or a default rotation.
	slots = h.want(owner, "PUT", poolPath, map[string]any{"slot": map[string]any{"name": "limited"}, "credential": "list-only"}, withMatch(slots, map[string]string{"Idempotency-Key": "list-only"}), 200)
	h.want(owner, "POST", poolPath+"/validate", nil, nil, 422)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	rotation := h.want(owner, "POST", path+"/credentials", map[string]any{"credential": "list-only"}, withMatch(detail, map[string]string{"Idempotency-Key": "rotate-list-only"}), 422)
	if problemCode(t, rotation) != "credential_invalid" {
		t.Fatalf("list-only credential accepted during rotation: %v", rotation)
	}
	after := h.want(owner, "GET", path, nil, nil, 200)
	if after["etag"] != detail["etag"] {
		t.Fatal("rejected rotation changed the draft")
	}
	slots = h.want(owner, "PUT", poolPath, map[string]any{"slot": map[string]any{"name": "limited", "allowed_models": []string{"disabled"}}}, withMatch(slots, map[string]string{"Idempotency-Key": "empty-scope"}), 200)
	h.want(owner, "POST", poolPath+"/validate", nil, nil, 422)
}

func TestSlotValidationRejectsDraftChangesDuringTheProbe(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	vendor := newVendor(t)
	vendor.accept("slow")
	started, released := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	var blocked atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer slow" && blocked.CompareAndSwap(false, true) {
			close(started)
			select {
			case <-released:
			case <-r.Context().Done():
				return
			}
		}
		vendor.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	t.Cleanup(release)
	created := h.want(owner, "POST", "/api/v3/providers", map[string]any{
		"name": "Concurrent validation", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	path := "/api/v3/providers/" + created["id"].(string)
	listPath := path + "/credential-slots"
	slots := h.want(owner, "GET", listPath, nil, nil, 200)
	slotID := uuid.NewString()
	slotPath := listPath + "/" + slotID
	slots = h.want(owner, "PUT", slotPath, map[string]any{"slot": map[string]any{"name": "slow"}, "credential": "slow"}, withMatch(slots, map[string]string{"Idempotency-Key": "pool"}), 200)
	validator := &browser{CSRF: owner.CSRF, Cookies: map[string]*http.Cookie{}}
	for name, cookie := range owner.Cookies {
		validator.Cookies[name] = cookie
	}
	result := make(chan int, 1)
	go func() {
		defer close(result)
		status, _, _ := h.request(validator, "POST", slotPath+"/validate", nil, nil)
		result <- status
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("validation did not reach the upstream")
	}
	slots = h.want(owner, "PUT", slotPath, map[string]any{"slot": map[string]any{"name": "rotated"}, "credential": "invalid"}, withMatch(slots, map[string]string{"Idempotency-Key": "rotate"}), 200)
	h.want(owner, "POST", slotPath+"/validate", nil, nil, 422)
	release()
	select {
	case status := <-result:
		if status != http.StatusPreconditionFailed {
			t.Fatalf("stale probe returned %d, want 412", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale probe did not finish")
	}
	slots = h.want(owner, "GET", listPath, nil, nil, 200)
	if slots["health"].(map[string]any)[slotID].(map[string]any)["validated_at"] != nil {
		t.Fatal("stale successful probe validated a rotated credential")
	}
}

func TestCertificationRejectsMalformedUnaryChoices(t *testing.T) {
	for _, choices := range []string{`[{}]`, `[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"},{}]`} {
		t.Run(choices, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					Stream bool `json:"stream"`
				}
				json.NewDecoder(r.Body).Decode(&input)
				if r.URL.Path == "/v1/responses" {
					writeResponsesFixture(w, vendorModel, "OK", input.Stream)
					return
				}
				if input.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				} else {
					fmt.Fprintf(w, `{"choices":%s}`, choices)
				}
			}))
			t.Cleanup(up.Close)
			created := h.want(owner, "POST", "/api/v3/providers", map[string]any{
				"name": "Malformed certification", "model": vendorModel, "credential": vendorSecret,
				"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
			}, map[string]string{"Idempotency-Key": "provider"}, 201)
			path := "/api/v3/providers/" + created["id"].(string)
			models := h.want(owner, "GET", path+"/models", nil, nil, 200)
			modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
			result := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(created), 200)
			if result["status"] != "partial" || result["certified_count"] != float64(1) || result["results"].([]any)[0].(map[string]any)["error_code"] != "provider_protocol_error" {
				t.Fatalf("malformed completion certified: %v", result)
			}
			detail := h.want(owner, "GET", path, nil, nil, 200)
			h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "reject"}), 422)
		})
	}
}
