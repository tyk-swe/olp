//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestCredentialSlotWritesRequireCurrentETag(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Slot preconditions", "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"}, "credential": vendorSecret}, map[string]string{"Idempotency-Key": "provider"}, 201)
	providerPath := "/api/v1/providers/" + created["id"].(string)
	listPath := providerPath + "/credential-slots"
	original := h.want(owner, "GET", listPath, nil, nil, 200)
	slotID := original["items"].([]any)[0].(map[string]any)["id"].(string)
	slotPath := listPath + "/" + slotID
	input := map[string]any{"slot": map[string]any{"name": "default", "allowed_routes": []string{routeSlug}, "allowed_api_keys": []string{uuid.NewString()}, "allowed_models": []string{vendorModel}}, "credential": "replacement"}
	if problemCode(t, h.want(owner, "PUT", slotPath, input, map[string]string{"Idempotency-Key": "missing-precondition"}, 428)) != "precondition_required" {
		t.Fatal("slot writes must require If-Match")
	}
	if problemCode(t, h.want(owner, "PUT", slotPath, input, withMatch(created, map[string]string{"Idempotency-Key": "provider-precondition"}), 412)) != "etag_mismatch" {
		t.Fatal("the provider ETag must not substitute for the slot-list ETag")
	}
	saved := h.want(owner, "PUT", slotPath, input, withMatch(original, map[string]string{"Idempotency-Key": "save"}), 200)
	if saved["etag"] == original["etag"] {
		t.Fatal("slot mutation did not advance its ETag")
	}
	staleInput := map[string]any{"slot": map[string]any{"name": "stale editor"}, "credential": "stale-credential"}
	for _, id := range []string{slotID, uuid.NewString()} {
		if problemCode(t, h.want(owner, "PUT", listPath+"/"+id, staleInput, withMatch(original, map[string]string{"Idempotency-Key": "stale-" + id}), 412)) != "etag_mismatch" {
			t.Fatal("a stale editor changed the credential pool")
		}
	}
	current := h.want(owner, "GET", listPath, nil, nil, 200)
	if !reflect.DeepEqual(current, saved) {
		t.Fatalf("rejected edits changed credentials or restrictions: %v, want %v", current, saved)
	}
	credentials := h.want(owner, "GET", providerPath+"/credentials", nil, nil, 200)
	if len(credentials["items"].([]any)) != 2 {
		t.Fatalf("rejected edits stored credentials: %v", credentials)
	}
}

func TestCredentialSlotRotationReplaysAndReportsPublishedCredential(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Slot rotation", "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"}, "credential": vendorSecret, "model": vendorModel}, map[string]string{"Idempotency-Key": "provider"}, 201)
	providerID := created["id"].(string)
	providerPath := "/api/v1/providers/" + providerID
	listPath := providerPath + "/credential-slots"
	original := h.want(owner, "GET", listPath, nil, nil, 200)
	slot := original["items"].([]any)[0].(map[string]any)
	slotID, originalCredential := slot["id"].(string), slot["credential_version_id"].(string)
	if original["health"].(map[string]any)[slotID].(map[string]any)["active_credential_version_id"] != nil {
		t.Fatal("a draft credential was reported active before publication")
	}
	models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	certified := h.want(owner, "POST", providerPath+"/models/"+modelID+"/certify", nil, etagHeader(created), 200)
	if certified["status"] != "certified" {
		t.Fatalf("certification %v", certified)
	}
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "activate"}), 200)
	original = h.want(owner, "GET", listPath, nil, nil, 200)
	if original["health"].(map[string]any)[slotID].(map[string]any)["active_credential_version_id"] != originalCredential {
		t.Fatal("publication did not report the selected credential as active")
	}

	slotPath := listPath + "/" + slotID
	input := map[string]any{"slot": map[string]any{"name": "default"}, "credential": "replacement"}
	headers := withMatch(original, map[string]string{"Idempotency-Key": "rotate-slot"})
	status, rotated, replyHeaders := h.request(owner, "PUT", slotPath, input, headers)
	if status != 200 {
		t.Fatalf("rotation status %d: %v", status, rotated)
	}
	replacement := rotated["items"].([]any)[0].(map[string]any)["credential_version_id"].(string)
	if replacement == originalCredential || rotated["health"].(map[string]any)[slotID].(map[string]any)["active_credential_version_id"] != originalCredential {
		t.Fatalf("staging a credential changed the active version: %v", rotated)
	}
	// A later edit must remain intact when the original rotation is retried.
	later := h.want(owner, "PUT", slotPath, map[string]any{"slot": map[string]any{"name": "restricted", "allowed_routes": []string{routeSlug}}}, withMatch(rotated, map[string]string{"Idempotency-Key": "restrict-slot"}), 200)
	status, replayed, replayHeaders := h.request(owner, "PUT", slotPath, input, headers)
	if status != 200 || !reflect.DeepEqual(replayed, rotated) || replayHeaders.Get("ETag") != replyHeaders.Get("ETag") {
		t.Fatalf("rotation retry did not replay the original response: status %d body %v headers %v", status, replayed, replayHeaders)
	}
	if problemCode(t, h.want(owner, "PUT", slotPath, map[string]any{"slot": map[string]any{"name": "default"}, "credential": "different"}, headers, 409)) != "idempotency_conflict" {
		t.Fatal("rotation replay accepted a different credential")
	}
	if problemCode(t, h.want(owner, "PUT", slotPath, input, withMatch(later, map[string]string{"Idempotency-Key": "rotate-slot"}), 409)) != "idempotency_conflict" {
		t.Fatal("rotation replay accepted a different precondition")
	}
	if problemCode(t, h.want(owner, "PUT", listPath+"/"+uuid.NewString(), input, headers, 409)) != "idempotency_conflict" {
		t.Fatal("rotation replay accepted a different slot")
	}
	if problemCode(t, h.want(owner, "PUT", slotPath, input, etagHeader(later), 400)) != "idempotency_key_required" {
		t.Fatal("slot rotation must require an idempotency key")
	}
	current := h.want(owner, "GET", listPath, nil, nil, 200)
	if !reflect.DeepEqual(current, later) {
		t.Fatalf("rotation replay overwrote a newer edit: %v, want %v", current, later)
	}
	credentials := h.want(owner, "GET", providerPath+"/credentials", nil, nil, 200)
	if len(credentials["items"].([]any)) != 2 {
		t.Fatalf("rotation retry created another credential: %v", credentials)
	}
	newSlot := uuid.NewString()
	staged := h.want(owner, "PUT", listPath+"/"+newSlot, map[string]any{"slot": map[string]any{"name": "sibling", "credential_version_id": replacement}}, withMatch(current, map[string]string{"Idempotency-Key": "new-slot"}), 200)
	if staged["health"].(map[string]any)[newSlot].(map[string]any)["active_credential_version_id"] != nil {
		t.Fatal("a newly staged slot was reported active before publication")
	}
	h.refresh()
	serving := h.Runtime.Release().Snapshot.Providers[providerID].Slots
	if len(serving) != 1 || *serving[0].CredentialID != originalCredential {
		t.Fatalf("draft slot edits changed serving credentials: %+v", serving)
	}
	up.accept("replacement")
	for _, id := range []string{slotID, newSlot} {
		h.want(owner, "POST", listPath+"/"+id+"/validate", nil, nil, 200)
	}
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "activate-rotation"}), 200)
	active := h.want(owner, "GET", listPath, nil, nil, 200)
	for _, id := range []string{slotID, newSlot} {
		if active["health"].(map[string]any)[id].(map[string]any)["active_credential_version_id"] != replacement {
			t.Fatalf("published slot %s has the wrong active credential: %v", id, active)
		}
	}
}

func TestCredentialHistoryTracksEveryDraftAndPublishedSlot(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := newVendor(t)
	up.accept(vendorRotated)
	up.accept("replacement")
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Credential history", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": up.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	path := "/api/v1/providers/" + created["id"].(string)
	slots := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	defaultCredential := slots["items"].([]any)[0].(map[string]any)["credential_version_id"].(string)
	poolSlot := uuid.NewString()
	slots = h.want(owner, "PUT", path+"/credential-slots/"+poolSlot, map[string]any{"slot": map[string]any{"name": "pool"}, "credential": vendorRotated}, withMatch(slots, map[string]string{"Idempotency-Key": "pool"}), 200)
	credentialFor := func(slotID string) string {
		t.Helper()
		for _, raw := range slots["items"].([]any) {
			slot := raw.(map[string]any)
			if slot["id"] == slotID {
				return slot["credential_version_id"].(string)
			}
		}
		t.Fatalf("missing slot %s", slotID)
		return ""
	}
	poolCredential := credentialFor(poolSlot)
	assertFlags := func(want map[string][2]bool) {
		t.Helper()
		history := h.want(owner, "GET", path+"/credentials", nil, nil, 200)
		items := history["items"].([]any)
		if len(items) != len(want) {
			t.Fatalf("credential history has missing or duplicate rows: %v", history)
		}
		got := map[string][2]bool{}
		for _, raw := range items {
			row := raw.(map[string]any)
			got[row["id"].(string)] = [2]bool{row["active"].(bool), row["draft_selected"].(bool)}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("credential active/draft flags %v, want %v", got, want)
		}
	}
	assertFlags(map[string][2]bool{defaultCredential: {false, true}, poolCredential: {false, true}})
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	certification := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certification["status"] != "certified" {
		t.Fatalf("fixture certification: %v", certification)
	}
	publish := func(key string) {
		t.Helper()
		detail := h.want(owner, "GET", path, nil, nil, 200)
		h.want(owner, "POST", path+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": key}), 200)
	}
	h.want(owner, "POST", path+"/credential-slots/"+poolSlot+"/validate", nil, nil, 200)
	publish("publish-pool")
	assertFlags(map[string][2]bool{defaultCredential: {true, true}, poolCredential: {true, true}})
	slots = h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	slots = h.want(owner, "PUT", path+"/credential-slots/"+poolSlot, map[string]any{"slot": map[string]any{"name": "pool"}, "credential": "replacement"}, withMatch(slots, map[string]string{"Idempotency-Key": "rotate-pool"}), 200)
	replacement := credentialFor(poolSlot)
	assertFlags(map[string][2]bool{defaultCredential: {true, true}, poolCredential: {true, false}, replacement: {false, true}})
	// Sharing a credential across slots must still produce one history row.
	sharedSlot := uuid.NewString()
	slots = h.want(owner, "PUT", path+"/credential-slots/"+sharedSlot, map[string]any{"slot": map[string]any{"name": "shared", "credential_version_id": replacement}}, withMatch(slots, map[string]string{"Idempotency-Key": "shared"}), 200)
	assertFlags(map[string][2]bool{defaultCredential: {true, true}, poolCredential: {true, false}, replacement: {false, true}})
	for _, id := range []string{poolSlot, sharedSlot} {
		h.want(owner, "POST", path+"/credential-slots/"+id+"/validate", nil, nil, 200)
	}
	publish("publish-rotation")
	assertFlags(map[string][2]bool{defaultCredential: {true, true}, poolCredential: {false, false}, replacement: {true, true}})
}

func TestProviderInventoryAvailabilityTracksPublishedModels(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/v1/responses" {
			writeResponsesFixture(w, input.Model, "ok", false)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`, input.Model)
	}))
	t.Cleanup(up.Close)
	configuration := func(endpoint string) map[string]any {
		return map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": endpoint}
	}
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Inventory publication", "configuration": configuration(up.URL + "/v1"), "credential": vendorSecret, "model": "first"}, map[string]string{"Idempotency-Key": "provider"}, 201)
	providerPath := "/api/v1/providers/" + created["id"].(string)
	certify := func(name string) string {
		t.Helper()
		models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)
		var id string
		for _, item := range models["items"].([]any) {
			m := item.(map[string]any)
			if m["upstream_model"] == name {
				id = m["id"].(string)
			}
		}
		if id == "" {
			t.Fatalf("missing model %s", name)
		}
		detail := h.want(owner, "GET", providerPath, nil, nil, 200)
		modelPath := providerPath + "/models/" + id
		detail = h.want(owner, "PATCH", modelPath, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}}, etagHeader(detail), 200)
		certified := h.want(owner, "POST", modelPath+"/certify", nil, etagHeader(detail), 200)
		if certified["status"] != "certified" || certified["certified_count"] != float64(1) {
			t.Fatalf("model %s certification: %v", name, certified)
		}
		return id
	}
	assertAvailability := func(want map[string]bool) {
		t.Helper()
		for _, query := range []string{"", "?surface=openai"} {
			inventory := h.want(owner, "GET", "/api/v1/provider-models"+query, nil, nil, 200)
			got := map[string]bool{}
			for _, item := range inventory["items"].([]any) {
				entry := item.(map[string]any)
				got[entry["model"].(map[string]any)["upstream_model"].(string)] = entry["available"].(bool)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("inventory%s availability %v, want %v", query, got, want)
			}
		}
	}
	firstID := certify("first")
	assertAvailability(map[string]bool{"first": false})
	detail := h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "publish-first"}), 200)
	assertAvailability(map[string]bool{"first": true})
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/discovery", map[string]any{"models": []any{map[string]any{"upstream_model": "second", "display_name": "Second"}}}, etagHeader(detail), 200)
	secondID := certify("second")
	assertAvailability(map[string]bool{"first": true, "second": false})
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	detail = h.want(owner, "PATCH", providerPath+"/models/"+firstID, map[string]any{"enabled": false}, etagHeader(detail), 200)
	assertAvailability(map[string]bool{"first": true, "second": false})
	slots := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)
	slotID := slots["items"].([]any)[0].(map[string]any)["id"].(string)
	h.want(owner, "POST", providerPath+"/credential-slots/"+slotID+"/validate", nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, map[string]string{"Idempotency-Key": "publish-second"}), 200)
	assertAvailability(map[string]bool{"first": false, "second": true})
	// A route can keep serving the active model while an older provider
	// revision becomes the draft, even when their model sets differ.
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets": []any{map[string]any{"provider_model_id": secondID, "priority": 0, "weight": 1, "timeout_ms": 2000}},
	}, map[string]string{"Idempotency-Key": "draft"}, 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	route := h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, map[string]string{"Idempotency-Key": "route"}), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "restore inference"}, map[string]string{"Idempotency-Key": "key"}, 201)
	h.refresh()
	sequence := h.Runtime.Release().Sequence
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	detail = h.want(owner, "PATCH", providerPath, map[string]any{"name": "Inventory publication", "configuration": configuration(up.URL + "/draft/v1")}, etagHeader(detail), 200)
	if detail["certified_capability_count"] != float64(0) {
		t.Fatal("transport change did not invalidate draft certification")
	}
	assertAvailability(map[string]bool{"first": false, "second": true})
	detail = h.want(owner, "POST", providerPath+"/discovery", map[string]any{"models": []any{map[string]any{"upstream_model": "draft-only"}}}, etagHeader(detail), 200)
	restored := h.want(owner, "POST", providerPath+"/revisions/1/restore-as-draft", nil, withMatch(detail, map[string]string{"Idempotency-Key": "restore-older"}), 200)
	detail = restored["provider"].(map[string]any)
	if restored["credential_restored"] != false || detail["pending_activation"] != true {
		t.Fatalf("older draft restore: %v", restored)
	}
	assertAvailability(map[string]bool{"first": false, "second": true})
	models := h.want(owner, "GET", providerPath+"/models", nil, nil, 200)
	for _, raw := range models["items"].([]any) {
		model := raw.(map[string]any)
		first := model["upstream_model"] == "first"
		wantID := secondID
		if first {
			wantID = firstID
		}
		if model["id"] != wantID || model["enabled"] != first {
			t.Fatalf("restore changed a published identity or enabled the wrong model: %v", model)
		}
		if !first {
			for _, raw := range model["capabilities"].([]any) {
				if raw.(map[string]any)["source"] != "declared" {
					t.Fatalf("retained model kept evidence outside the restored revision: %v", model)
				}
			}
		}
	}
	live := h.want(owner, "GET", "/api/v1/routes/"+route["route_id"].(string), nil, nil, 200)
	if live["latest_revision"].(map[string]any)["targets"].([]any)[0].(map[string]any)["available"] != true {
		t.Fatalf("draft restore hid the published route target: %v", live)
	}
	currentDraft := h.want(owner, "GET", draftPath, nil, nil, 200)
	h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(currentDraft), 200)
	decisions := h.list(owner, "POST", "/api/v1/routing/simulate", map[string]any{"operation": map[string]any{"request": map[string]any{"route": routeSlug}}, "surface": "openai", "mode": "unary", "api_key_id": key["id"]}, nil, 200)
	if len(decisions) != 1 || decisions[0].(map[string]any)["eligible"] != true {
		t.Fatalf("restore changed routing simulation: %v", decisions)
	}
	h.refresh()
	status, result, _ := h.gateway("POST", "/v1/chat/completions", key["secret"].(string), map[string]any{"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if status != http.StatusOK || result["model"] != routeSlug || h.Runtime.Release().Sequence != sequence {
		t.Fatalf("draft restore changed serving state: %d %v", status, result)
	}
	detail = h.want(owner, "POST", providerPath+"/restore-as-draft", nil, withMatch(detail, map[string]string{"Idempotency-Key": "restore-active"}), 200)
	if detail["pending_activation"] != false {
		t.Fatalf("restoring the active revision did not produce a clean draft: %v", detail)
	}
	assertAvailability(map[string]bool{"first": false, "second": true})
	h.want(owner, "POST", providerPath+"/disable", nil, withMatch(detail, map[string]string{"Idempotency-Key": "disable"}), 200)
	assertAvailability(map[string]bool{"first": false, "second": false})
}
