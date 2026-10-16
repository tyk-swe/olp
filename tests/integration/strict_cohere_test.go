//go:build integration

package integration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
)

const cohereTinyPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// Cohere's native v2 endpoint is distinct from its existing /compatibility/v1
// preset. Provision through the same public management seam as other unary
// operations, with the native v2 base path and a separately registered profile.
func publishCohereOperation(t *testing.T, h *accessHarness, owner *browser, f *operationFixture, profileID, operation, probe string) (string, string) {
	t.Helper()
	profile, err := connectors.LookupProfile(profileID, "1")
	if err != nil {
		t.Fatal(err)
	}
	configuration := map[string]any{"kind": profile.Kind, "profile_id": profile.ID, "profile_revision": "1", "endpoint": f.URL + "/v2", "auth_mode": "api_key", "options": map[string]any{"vendor_id": "cohere-native-v2"}}
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Cohere native " + uuid.NewString(), "configuration": configuration, "model": vendorModel, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + provider["id"].(string)
	model := h.want(owner, "GET", path+"/models", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	provider = h.want(owner, "PATCH", path+"/models/"+model, map[string]any{"enabled": true, "capabilities": operationCapabilities(profileID, operation, "native")}, etagHeader(provider), 200)
	f.mu.Lock()
	original := f.response
	f.mu.Unlock()
	f.result(probe)
	certified := h.want(owner, "POST", path+"/models/"+model+"/certify", nil, etagHeader(provider), 200)
	if certified["status"] == "failed" {
		t.Fatalf("Cohere native certification: %v", certified)
	}
	provider = h.want(owner, "GET", path, nil, nil, 200)
	f.mu.Lock()
	f.response, f.calls = original, nil
	f.mu.Unlock()
	h.want(owner, "POST", path+"/activate", nil, withMatch(provider, idem(uuid.NewString())), 200)
	slug := "cohere-" + uuid.NewString()
	draftInput := fidelityDraft(slug, provider["id"])
	draftInput["operations"] = []string{operation}
	draftInput["fidelity"] = map[string]any{"mode": "strict"}
	if f.policy != nil {
		draftInput["content_policy"] = f.policy
	}
	draft := h.want(owner, "POST", "/api/v1/route-drafts", draftInput, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Cohere native", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem(uuid.NewString()), 201)
	h.refresh()
	return slug, key["secret"].(string)
}
