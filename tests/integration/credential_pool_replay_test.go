//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestLargeCredentialPoolWritesReplayOriginalResponse(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Large credential pool", "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	path := "/api/v1/providers/" + created["id"].(string) + "/credential-slots"
	pool := h.want(owner, "GET", path, nil, nil, 200)
	defaultID := pool["items"].([]any)[0].(map[string]any)["id"].(string)
	models := make([]string, 2000)
	for i := range models {
		models[i] = fmt.Sprintf("allowed-model-%05d", i)
	}
	var saved, savedInput map[string]any
	var savedHeaders map[string]string
	var savedETag, savedPath string
	for i, slotID := range []string{defaultID, uuid.NewString()} {
		input := map[string]any{"slot": map[string]any{"name": fmt.Sprintf("slot-%d", i), "allowed_models": models}}
		encoded, err := json.Marshal(input)
		if err != nil || len(encoded) >= 65536 || len(encoded) <= 32768 {
			t.Fatalf("fixture must fit one request and exceed half the old replay cap: bytes=%d err=%v", len(encoded), err)
		}
		headers := withMatch(pool, map[string]string{"Idempotency-Key": fmt.Sprintf("large-slot-%d", i)})
		status, result, responseHeaders := h.request(owner, "PUT", path+"/"+slotID, input, headers)
		if status != 200 {
			t.Fatalf("valid slot %d failed: %d %v", i, status, result)
		}
		pool = result
		if i == 1 {
			saved, savedInput, savedHeaders = result, input, headers
			savedETag, savedPath = responseHeaders.Get("ETag"), path+"/"+slotID
		}
	}
	encoded, err := json.Marshal(saved)
	if err != nil || len(encoded) <= 65536 || len(saved["items"].([]any)) != 2 {
		t.Fatalf("fixture did not cross the replay limit: bytes=%d err=%v", len(encoded), err)
	}
	// Change current state before replaying the large result with its original
	// precondition. Replay must return that result without repeating the write.
	updated := h.want(owner, "PUT", path+"/"+defaultID, map[string]any{"slot": map[string]any{"name": "later-edit", "allowed_models": models}}, withMatch(pool, map[string]string{"Idempotency-Key": "later-edit"}), 200)
	status, replayed, responseHeaders := h.request(owner, "PUT", savedPath, savedInput, savedHeaders)
	if status != 200 || responseHeaders.Get("ETag") != savedETag || !reflect.DeepEqual(replayed, saved) {
		t.Fatalf("large response was not replayed exactly: status=%d etag=%s", status, responseHeaders.Get("ETag"))
	}
	current := h.want(owner, "GET", path, nil, nil, 200)
	if !reflect.DeepEqual(current, updated) {
		t.Fatal("replay changed the current credential pool")
	}
	conflict := h.want(owner, "PUT", savedPath, map[string]any{"slot": map[string]any{"name": "conflicting-edit"}}, savedHeaders, 409)
	if problemCode(t, conflict) != "idempotency_conflict" {
		t.Fatalf("large replay lost request binding: %v", conflict)
	}
}
