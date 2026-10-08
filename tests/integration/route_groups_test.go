//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRouteGroupsRefreshBothPublicScopesAcrossGateways(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Route groups"}, idem("project"), 201)
	id := project["id"].(string)
	path := "/api/v1/projects/" + id + "/route-groups"
	initial := h.want(owner, "GET", path, nil, nil, 200)
	for _, body := range []any{map[string]any{}, map[string]any{"groups": nil}, map[string]any{"groups": map[string]any{"bad name": []string{}}}, map[string]any{"groups": map[string]any{"valid": []string{"a", "a"}}}} {
		h.want(owner, "PUT", path, body, etagHeader(initial), 422)
	}
	h.want(owner, "PUT", path, map[string]any{"groups": map[string]any{"production": []string{"group-a"}, "empty": []string{}}}, etagHeader(initial), 200)
	fixture := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Grouped provider", fixture.URL, &id, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	for _, slug := range []string{"group-a", "group-b"} {
		draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": id, "slug": slug, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem(slug), 201)
		draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
		validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
		h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, idem("activate-"+slug)), 200)
	}
	input := map[string]any{"name": "Grouped key", "project_id": id, "scopes": []string{"inference", "models_read"}, "allowed_routes": []string{}, "allowed_route_groups": []string{"missing"}}
	h.want(owner, "POST", "/api/v1/api-keys", input, idem("missing-group"), 422)
	input["allowed_route_groups"] = []string{"production"}
	input["project_id"] = nil
	h.want(owner, "POST", "/api/v1/api-keys", input, idem("unassigned"), 422)
	input["project_id"] = id
	key := h.want(owner, "POST", "/api/v1/api-keys", input, idem("grouped-key"), 201)
	secret := key["secret"].(string)
	keyPath := "/api/v1/api-keys/" + key["id"].(string)
	detail := h.want(owner, "GET", keyPath, nil, nil, 200)
	if detail["allowed_route_groups"].([]any)[0] != "production" {
		t.Fatal("group reference was lost")
	}
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	replica.refresh()
	check := func(allowed, denied string) {
		t.Helper()
		if status, raw := attributionCall(t, replica, allowed, secret, nil); status != 200 {
			t.Fatalf("allowed %s: %d %s", allowed, status, raw)
		}
		if status, raw := attributionCall(t, replica, denied, secret, nil); status != 403 {
			t.Fatalf("denied %s: %d %s", denied, status, raw)
		}

		for _, tc := range []struct {
			slug     string
			eligible bool
		}{{allowed, true}, {denied, false}} {
			decisions := h.list(owner, "POST", "/api/v1/routing/simulate", map[string]any{"operation": map[string]any{"operation": "generation", "request": map[string]any{"route": tc.slug}}, "surface": "openai", "mode": "unary", "seed": "group-member", "api_key_id": key["id"]}, nil, 200)
			if len(decisions) != 1 || decisions[0].(map[string]any)["eligible"] != tc.eligible {
				t.Fatalf("simulation differs from authorization: %v", decisions)
			}
		}
		models := replica.machineWant(secret, "GET", "/v1/models", nil, nil, 200)["data"].([]any)
		if len(models) != 1 || models[0].(map[string]any)["id"] != allowed {
			t.Fatalf("model visibility: %v", models)
		}
	}
	check("group-a", "group-b")
	current := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"groups": map[string]any{"production": []string{"group-b"}}}, etagHeader(current), 200)
	replica.refresh()
	check("group-b", "group-a")
	// An explicit route grants the union, without replacing the group reference.
	h.want(owner, "PATCH", keyPath, map[string]any{"allowed_routes": []string{"group-a"}}, etagHeader(detail), 200)
	replica.refresh()
	for _, slug := range []string{"group-a", "group-b"} {
		if status, raw := attributionCall(t, replica, slug, secret, nil); status != 200 {
			t.Fatalf("union: %d %s", status, raw)
		}
	}
	current = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"groups": map[string]any{}}, etagHeader(current), 200)

	replica.refresh()
	check("group-a", "group-b")
	foreign := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Foreign groups"}, idem("foreign-project"), 201)
	foreignPath := "/api/v1/projects/" + foreign["id"].(string) + "/route-groups"
	foreignDetail := h.want(owner, "GET", foreignPath, nil, nil, 200)
	h.want(owner, "PUT", foreignPath, map[string]any{"groups": map[string]any{"steal": []string{"group-a"}}}, etagHeader(foreignDetail), 422)
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	for _, value := range doc["projects"].([]any) {
		entry := value.(map[string]any)
		if entry["name"] == "Foreign groups" {
			entry["route_groups"] = map[string]any{"steal": []string{"group-a"}}
		}
	}
	body := map[string]any{"document": doc}
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 422)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("foreign-promotion"), 422)
}

func TestRouteGroupPromotionRequiresKeysAndPreservesReferences(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Portable groups"}, idem("project"), 201)
	path := "/api/v1/projects/" + project["id"].(string) + "/route-groups"
	initial := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"groups": map[string]any{"production": []string{"future"}}}, etagHeader(initial), 200)
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	entry := doc["projects"].([]any)[0].(map[string]any)
	if _, ok := entry["route_groups"]; !ok {
		t.Fatal("export omitted route groups")
	}
	entry["route_groups"] = map[string]any{"production": []string{"next"}}
	body := map[string]any{"document": doc}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Configure", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("denied"), 403)
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("apply"), 200)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("unchanged"), 200)
	saved := h.want(owner, "GET", path, nil, nil, 200)
	raw, _ := json.Marshal(saved["groups"])
	if string(raw) != `{"production":["next"]}` {
		t.Fatalf("promoted groups: %s", raw)
	}
}

func TestRouteGroupRemovalClosesEstablishedRealtimeSession(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Live groups"}, idem("project"), 201)
	id := project["id"].(string)
	fixture := newStrictRealtimeFixture(t, "openai")
	slug, key := provisionStrictRealtimeInProject(t, h, owner, "openai", fixture.URL+"/v1", nil, &id)
	path := "/api/v1/projects/" + id + "/route-groups"
	initial := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"groups": map[string]any{"live": []string{slug}}}, etagHeader(initial), 200)
	authority, err := h.Runtime.Authenticate(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := "/api/v1/api-keys/" + authority.ID
	detail := h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "PATCH", keyPath, map[string]any{"allowed_routes": []string{}, "allowed_route_groups": []string{"live"}}, etagHeader(detail), 200)
	h.refresh()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(h.HTTP.URL, "http://", "ws://", 1)+"/v1/realtime?model="+slug, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	current := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"groups": map[string]any{}}, etagHeader(current), 200)
	h.refresh()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("removed group did not close session: %v", err)
	}
}
