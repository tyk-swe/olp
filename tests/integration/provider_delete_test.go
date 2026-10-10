//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProviderDeletionRequiresUnusedDraftAndCleansOwnedSeals(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	vendor := newVendor(t)
	projectID := createProject(h, owner, "Provider lifecycle")
	draft := createScopedProvider(h, owner, "Unused draft", vendor.Server.URL+"/v1", projectID, 201)
	path := "/api/v1/providers/" + draft["id"].(string)
	h.want(owner, "DELETE", path, nil, idem(uuid.NewString()), 428)
	h.want(owner, "DELETE", path, nil, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + uuid.NewString() + `"`}, 412)
	var credentialID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT credential_id::text FROM olp.provider_slots WHERE provider_id=$1 AND is_default", draft["id"]).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	headers := withMatch(draft, idem(uuid.NewString()))
	h.want(owner, "DELETE", path, nil, headers, 204)
	h.want(owner, "DELETE", path, nil, headers, 204)
	h.want(owner, "GET", path, nil, nil, 404)
	var present bool
	if err := h.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp.secrets WHERE id=$1)", credentialID).Scan(&present); err != nil || present {
		t.Fatalf("deleted provider retains sealed credential: %v", err)
	}
	published := createScopedProvider(h, owner, "Published provider", vendor.Server.URL+"/v1", projectID, 201)
	activateScopedProvider(h, owner, published)
	publishedPath := "/api/v1/providers/" + published["id"].(string)
	current := h.want(owner, "GET", publishedPath, nil, nil, 200)
	h.want(owner, "DELETE", publishedPath, nil, withMatch(current, idem(uuid.NewString())), 409)
	disabled := h.want(owner, "POST", publishedPath+"/disable", nil, withMatch(current, idem(uuid.NewString())), 200)
	h.want(owner, "DELETE", publishedPath, nil, withMatch(disabled, idem(uuid.NewString())), 409)
	retained := createScopedProvider(h, owner, "Referenced draft", vendor.Server.URL+"/v1", projectID, 201)
	h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": "retained-provider", "project_id": projectID, "operations": []string{"generation"}, "overall_timeout_ms": 3000, "max_attempts": 1,
		"fidelity": map[string]string{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": retained["id"], "provider_model": vendorModel, "weight": 1, "priority": 0, "timeout_ms": 2000}},
	}, idem(uuid.NewString()), 201)
	h.want(owner, "DELETE", "/api/v1/providers/"+retained["id"].(string), nil, withMatch(retained, idem(uuid.NewString())), 409)
	if err := h.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp.providers WHERE id=$1)", retained["id"]).Scan(&present); err != nil || !present {
		t.Fatalf("draft reference did not protect provider: %v", err)
	}
	priced := createScopedProvider(h, owner, "Priced draft", vendor.Server.URL+"/v1", projectID, 201)
	price := repPrice("openai_compatible", vendorModel, "generation")
	price["provider_id"] = priced["id"]
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "prices": []any{price}}, idem(uuid.NewString()), 201)
	h.want(owner, "DELETE", "/api/v1/providers/"+priced["id"].(string), nil, withMatch(priced, idem(uuid.NewString())), 409)
	if err := h.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp.prices WHERE provider_id=$1)", priced["id"]).Scan(&present); err != nil || !present {
		t.Fatalf("deletion erased provider-specific price history: %v", err)
	}
	var audits int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action='provider.delete' AND resource_id=$1 AND project_id=$2", draft["id"], projectID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("successful deletion audit count=%d: %v", audits, err)
	}
}
