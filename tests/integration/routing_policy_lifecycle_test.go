//go:build integration

package integration_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestRoutingPolicyRemovalPreservesFreshPreconditionsAndParentProof(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Policy parent"}, idem(uuid.NewString()), 201)
	keyPath := "/api/v1/api-keys/" + key["id"].(string)
	projectID := createProject(h, owner, "Draft policy")
	routeID := createCatalogRoute(h, owner, projectID, "policy-draft")
	route := h.want(owner, "GET", "/api/v1/routes/"+routeID, nil, nil, 200)
	draftID := route["latest_revision"].(map[string]any)["source_draft_id"].(string)
	draftPath := "/api/v1/route-drafts/" + draftID
	for _, scope := range []struct{ name, id, parentPath string }{{"installation", uuid.Nil.String(), ""}, {"api-key", key["id"].(string), keyPath}, {"route-draft", draftID, draftPath}} {
		t.Run(scope.name, func(t *testing.T) {
			path := "/api/v1/routing-policies/" + scope.name + "/" + scope.id
			original := h.want(owner, "GET", path, nil, nil, 200)
			if original["configured"] != false {
				t.Fatal("unconfigured policy must expose inherited defaults")
			}
			policy := map[string]any{"allowed_strategies": []any{"weighted"}}
			created := h.want(owner, "PUT", path, policy, withMatch(original, idem(uuid.NewString())), 200)
			if created["configured"] != true {
				t.Fatal("configured policy was not declared")
			}
			h.want(owner, "DELETE", path, nil, idem(uuid.NewString()), 428)
			h.want(owner, "DELETE", path, nil, withMatch(original, idem(uuid.NewString())), 412)
			var parentBefore map[string]any
			if scope.parentPath != "" {
				parentBefore = h.want(owner, "GET", scope.parentPath, nil, nil, 200)
			}
			headers := withMatch(created, idem(uuid.NewString()))
			response, _ := h.do(owner, "DELETE", path, nil, headers)
			if response.StatusCode != http.StatusNoContent {
				t.Fatalf("removal status %d", response.StatusCode)
			}
			previous, current := response.Header.Get("OLP-Previous-Parent-ETag"), response.Header.Get("OLP-Parent-ETag")
			response.Body.Close()
			removed := h.want(owner, "GET", path, nil, nil, 200)
			if removed["configured"] != false || removed["etag"] == original["etag"] || removed["etag"] == created["etag"] {
				t.Fatal("removal reintroduced an old/default precondition")
			}
			if scope.parentPath != "" {
				parent := h.want(owner, "GET", scope.parentPath, nil, nil, 200)
				if previous != `"`+parentBefore["etag"].(string)+`"` || current != `"`+parent["etag"].(string)+`"` {
					t.Fatal("scoped removal did not prove its exact parent transition")
				}
			} else if previous != "" || current != "" {
				t.Fatal("installation policy has no independent parent transition")
			}
			h.want(owner, "PUT", path, policy, withMatch(original, idem(uuid.NewString())), 412)
			response, _ = h.do(owner, "DELETE", path, nil, headers)
			if response.StatusCode != 204 || response.Header.Get("OLP-Previous-Parent-ETag") != previous || response.Header.Get("OLP-Parent-ETag") != current {
				t.Fatal("removal replay changed its proof")
			}
			response.Body.Close()
			recreated := h.want(owner, "PUT", path, policy, withMatch(removed, idem(uuid.NewString())), 200)
			if recreated["configured"] != true || recreated["etag"] == removed["etag"] {
				t.Fatal("policy could not be recreated from fresh defaults")
			}
			h.want(owner, "DELETE", path, nil, withMatch(recreated, idem(uuid.NewString())), 204)
		})
	}
	var removals int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action='routing_policy.delete'").Scan(&removals); err != nil {
		t.Fatal(err)
	}
	if removals != 6 {
		t.Fatalf("replay duplicated successful audit: %d", removals)
	}
}
