//go:build integration

package integration_test

import (
	"testing"

	"github.com/google/uuid"
)

func TestCredentialSlotConditionsAreIndependentAndPreservePublishedVersions(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	vendor := newVendor(t)
	projectID := createProject(h, owner, "Slot resources")
	provider := createScopedProvider(h, owner, "Slot provider", vendor.Server.URL+"/v1", projectID, 201)
	activateScopedProvider(h, owner, provider)
	base := "/api/v1/providers/" + provider["id"].(string)
	pool := h.want(owner, "GET", base+"/credential-slots", nil, nil, 200)
	firstID, secondID := uuid.NewString(), uuid.NewString()
	firstPath := base + "/credential-slots/" + firstID
	secondPath := base + "/credential-slots/" + secondID
	pool = h.want(owner, "PUT", firstPath, map[string]any{"slot": map[string]any{"name": "First"}, "credential": vendorSecret}, withMatch(pool, idem(uuid.NewString())), 200)
	first := h.want(owner, "GET", firstPath, nil, nil, 200)
	pool = h.want(owner, "PUT", secondPath, map[string]any{"slot": map[string]any{"name": "Second"}, "credential": vendorSecret}, withMatch(pool, idem(uuid.NewString())), 200)
	second := h.want(owner, "GET", secondPath, nil, nil, 200)
	// The second slot changes the pool ETag, not the first slot's precondition.
	h.want(owner, "PUT", firstPath, map[string]any{"slot": map[string]any{"name": "First changed"}}, withMatch(first, idem(uuid.NewString())), 200)
	h.want(owner, "PUT", firstPath, map[string]any{"slot": map[string]any{"name": "Stale overwrite"}}, withMatch(first, idem(uuid.NewString())), 412)
	unchanged := h.want(owner, "GET", secondPath, nil, nil, 200)
	if unchanged["etag"] != second["etag"] {
		t.Fatal("a sibling update changed the individual slot ETag")
	}
	for _, path := range []string{firstPath, secondPath} {
		pool = h.want(owner, "GET", base+"/credential-slots", nil, nil, 200)
		h.want(owner, "POST", path+"/validate", nil, withMatch(pool, nil), 200)
	}
	current := h.want(owner, "GET", base, nil, nil, 200)
	h.want(owner, "POST", base+"/activate", nil, withMatch(current, idem(uuid.NewString())), 200)
	first = h.want(owner, "GET", firstPath, nil, nil, 200)
	credentialID := first["credential_version_id"].(string)
	h.want(owner, "DELETE", firstPath, nil, idem(uuid.NewString()), 428)
	headers := withMatch(first, idem(uuid.NewString()))
	h.want(owner, "DELETE", firstPath, nil, headers, 204)
	h.want(owner, "DELETE", firstPath, nil, headers, 204)
	h.want(owner, "GET", firstPath, nil, nil, 404)
	var retained bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.provider_credentials WHERE id=$1)
 AND EXISTS(SELECT 1 FROM olp.secrets WHERE id=$1)
 AND EXISTS(SELECT 1 FROM olp.provider_revisions r,jsonb_array_elements(r.slots) s WHERE r.provider_id=$2 AND s->>'credential_id'=$1::text)`, credentialID, provider["id"]).Scan(&retained); err != nil || !retained {
		t.Fatalf("published credential did not survive draft slot deletion: %v", err)
	}
	pool = h.want(owner, "GET", base+"/credential-slots", nil, nil, 200)
	var defaultID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.provider_slots WHERE provider_id=$1 AND is_default", provider["id"]).Scan(&defaultID); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "DELETE", base+"/credential-slots/"+defaultID, nil, withMatch(pool, idem(uuid.NewString())), 409)
}
