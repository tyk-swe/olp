//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestPlanInspectorShowsNegotiatedActionabilityWithoutCreatingWork(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	var calls atomic.Int64
	slug, _ := continuationBarrierFixtureOwner(t, h, owner, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		calls.Add(1)
		http.Error(w, "inspection must not dispatch", 500)
	})
	allowed := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "negotiated inspector", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": true}, idem(uuid.NewString()), 201)
	denied := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "disabled inspector", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "allow_provider_state": false}, idem(uuid.NewString()), 201)
	request := map[string]any{}
	if err := json.Unmarshal([]byte(strings.Replace(continuationInput, "ROUTE", slug, 1)), &request); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"operation": map[string]any{"operation": "generation", "request": request}, "surface": "openai", "mode": "streaming", "dialect": "openai-chat", "client_contract": continuationClientVersion, "api_key_id": allowed["id"], "seed": "no-inference"}
	decision := h.list(owner, "POST", "/api/v1/routing/simulate", input, nil, 200)[0].(map[string]any)
	inspection := decision["interaction"].(map[string]any)
	obligations := inspection["obligations"].(map[string]any)
	if decision["eligible"] != true || inspection["status"] != "admitted" || inspection["class"] != "qualified_interaction" || obligations["continuation"] != continuationClientVersion || obligations["submission"] != "client_submission_identity" || obligations["actionability"] != "encrypted_native_dependency_before_tool_bytes" || obligations["max_continuation_bytes"] != float64(4<<20) {
		t.Fatalf("inspector omitted negotiated barrier and bounded state: %v", decision)
	}
	encoded, _ := json.Marshal(decision)
	if strings.Contains(string(encoded), "Weather and time in Paris?") || strings.Contains(string(encoded), "opaque-fixture-signature") || strings.Contains(string(encoded), vendorSecret) {
		t.Fatal("inspector exposed private source or opaque dependency")
	}
	input["api_key_id"] = denied["id"]
	rejected := h.list(owner, "POST", "/api/v1/routing/simulate", input, nil, 200)[0].(map[string]any)
	assertInspectorRejection(t, rejected, "policy_conflict")
	input["api_key_id"] = allowed["id"]
	delete(input, "client_contract")
	rejected = h.list(owner, "POST", "/api/v1/routing/simulate", input, nil, 200)[0].(map[string]any)
	assertInspectorRejection(t, rejected, "state_carrier")
	input["client_contract"] = "future-v2"
	h.want(owner, "POST", "/api/v1/routing/simulate", input, nil, 422)
	var claims int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources WHERE route_slug=$1 AND kind='continuation'`, slug).Scan(&claims); err != nil || claims != 0 || calls.Load() != 0 {
		t.Fatalf("no-inference inspector created work: claims=%d provider=%d err=%v", claims, calls.Load(), err)
	}
}
