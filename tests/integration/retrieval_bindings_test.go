//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/retrieval"
)

// This scenario needs the repository database/process fixture and is left to
// CI. It verifies revision durability and project/key authority without a live
// cloud service or retaining any retrieved document content locally.
func TestRetrievalBindingsKeepExplicitGrantsAndImmutableHistory(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, http.MethodPost, "/api/v1/projects", map[string]any{"name": "retrieval team"}, idem(uuid.NewString()), 201)["id"].(string)
	foreignProject := h.want(owner, http.MethodPost, "/api/v1/projects", map[string]any{"name": "retrieval foreign"}, idem(uuid.NewString()), 201)["id"].(string)
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "search key", "project_id": project, "scopes": []string{"inference"}, "allow_provider_state": true}, idem(uuid.NewString()), 201)
	foreign := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "foreign key", "project_id": foreignProject, "scopes": []string{"inference"}, "allow_provider_state": true}, idem(uuid.NewString()), 201)
	var actor string
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.users WHERE role='owner' ORDER BY created_at LIMIT 1").Scan(&actor); err != nil {
		t.Fatal(err)
	}
	provider, target := uuid.NewString(), uuid.NewString()
	route := "cloud-retrieval"
	if _, err := h.Pool.Exec(t.Context(), "INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,'retrieval connection','azure_ai_search','draft','{}',$2,$3,$4,$5)", provider, uuid.NewString(), uuid.NewString(), actor, project); err != nil {
		t.Fatal(err)
	}
	targets, _ := json.Marshal([]map[string]any{{"id": target, "provider_id": provider, "provider_model": "documents", "provider_model_id": uuid.NewString(), "weight": 1, "priority": 0, "timeout_ms": 1000, "position": 0}})
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.route_drafts(id,slug,state,operations,overall_timeout_ms,max_attempts,targets,etag,created_by,project_id,fidelity) VALUES($1,$2,'draft','["retrieval"]',2000,1,$3,$4,$5,$6,'{"mode":"transformed"}')`, uuid.NewString(), route, targets, uuid.NewString(), actor, project); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"project_id": project, "name": "documents", "route": route, "target_id": target, "backend": "azure_ai_search", "backend_resource": "documents", "fields": map[string]any{"text": "content", "id": "key"}, "key_grants": []string{foreign["id"].(string)}, "enabled": true}
	h.want(owner, http.MethodPost, "/api/v1/retrieval-bindings", input, idem(uuid.NewString()), 422)
	input["key_grants"] = []string{key["id"].(string)}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET expires_at=now()-interval '1 hour' WHERE id=$1`, key["id"].(string)); err != nil {
		t.Fatal(err)
	}
	h.want(owner, http.MethodPost, "/api/v1/retrieval-bindings", input, idem(uuid.NewString()), 422)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET expires_at=NULL WHERE id=$1`, key["id"].(string)); err != nil {
		t.Fatal(err)
	}
	created := h.want(owner, http.MethodPost, "/api/v1/retrieval-bindings", input, idem(uuid.NewString()), 201)
	id := created["id"].(string)
	if created["provider_id"] != provider {
		t.Fatal("connection identity was not derived from the target")
	}
	input["enabled"] = false
	updated := h.want(owner, http.MethodPut, "/api/v1/retrieval-bindings/"+id, input, withMatch(created, idem(uuid.NewString())), 200)
	h.want(owner, http.MethodPut, "/api/v1/retrieval-bindings/"+id, input, withMatch(created, idem(uuid.NewString())), 412)
	if updated["revision_id"] == created["revision_id"] {
		t.Fatal("revision identity was reused")
	}
	history := h.want(owner, http.MethodGet, "/api/v1/retrieval-bindings/"+id+"/revisions/"+created["revision_id"].(string), nil, nil, 200)
	if definition := history["definition"].(map[string]any); definition["enabled"] != true {
		t.Fatal("update changed the original revision")
	}
	// Portable names must never turn a stale UUID grant into a replacement key.
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET expires_at=now()-interval '1 hour' WHERE id=$1`, key["id"].(string)); err != nil {
		t.Fatal(err)
	}
	h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "search key", "project_id": project, "scopes": []string{"inference"}, "allow_provider_state": true}, idem(uuid.NewString()), 201)
	for _, inactive := range []string{"expired", "revoked"} {
		if inactive == "revoked" {
			if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET expires_at=NULL,revoked_at=now() WHERE id=$1`, key["id"].(string)); err != nil {
				t.Fatal(err)
			}
		}
		problem := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, 422)
		fields, _ := problem["errors"].(map[string]any)
		if fields["retrieval_bindings.key_grants"] == nil {
			t.Fatalf("%s source grant exported as a different key: %v", inactive, problem)
		}
	}
	h.want(owner, http.MethodDelete, "/api/v1/retrieval-bindings/"+id, nil, withMatch(updated, idem(uuid.NewString())), 204)
	restored, err := retrieval.Load(t.Context(), h.Pool, id, false)
	if err != nil || restored.RetiredAt == nil || restored.ProviderID != provider {
		t.Fatalf("retirement lost history or connection pin: %+v %v", restored, err)
	}
	h.want(owner, http.MethodGet, "/api/v1/retrieval-bindings/"+id+"/revisions/"+created["revision_id"].(string), nil, nil, 200)
}

func TestRetrievalAzureSearchPricingScopes(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	h.want(owner, http.MethodPost, "/api/v1/pricing/discounts", map[string]any{
		"provider_kind": "azure_ai_search", "multiplier": "0.8",
	}, idem(uuid.NewString()), http.StatusCreated)
	card := h.want(owner, http.MethodPost, "/api/v1/rate-cards", map[string]any{
		"owner": map[string]any{"kind": "installation"},
	}, idem(uuid.NewString()), http.StatusCreated)
	h.want(owner, http.MethodPost, "/api/v1/rate-cards/"+card["id"].(string)+"/revisions", map[string]any{
		"markup": "1", "prices": []any{map[string]any{
			"provider_kind": "azure_ai_search", "model": "documents", "operation": "retrieval", "per_request": "0.01",
		}},
	}, idem(uuid.NewString()), http.StatusCreated)
}
