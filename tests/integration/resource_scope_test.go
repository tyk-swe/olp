//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/usage"
)

func idem(key string) map[string]string {
	return map[string]string{"Idempotency-Key": key}
}

func projectEtag(h *accessHarness, owner *browser, projectID string) map[string]string {
	h.t.Helper()
	return etagHeader(h.want(owner, "GET", "/api/v1/projects/"+projectID, nil, nil, 200))
}

func createProject(h *accessHarness, owner *browser, name string) string {
	h.t.Helper()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": name}, idem("project-"+name), 201)
	return project["id"].(string)
}

func addMember(h *accessHarness, owner *browser, projectID, userID, role string) {
	h.t.Helper()
	h.want(owner, "PUT", "/api/v1/projects/"+projectID+"/members/"+userID,
		map[string]any{"role": role}, projectEtag(h, owner, projectID), 200)
}

func createScopedProvider(h *accessHarness, b *browser, name, endpoint string, projectID any, want int) map[string]any {
	h.t.Helper()
	body := map[string]any{
		"name": name,
		"configuration": map[string]any{
			"kind": "openai_compatible", "auth_mode": "api_key",
			"endpoint": endpoint,
		},
		"credential": vendorSecret,
		"model":      vendorModel,
	}
	if projectID != nil {
		body["project_id"] = projectID
	}
	return h.want(b, "POST", "/api/v1/providers", body, idem("provider-"+name), want)
}

func activateScopedProvider(h *accessHarness, b *browser, provider map[string]any) {
	h.t.Helper()
	path := "/api/v1/providers/" + provider["id"].(string)
	probe := h.want(b, "POST", path+"/probe", nil, etagHeader(provider), 200)
	if probe["succeeded"] != true {
		h.t.Fatalf("probe must succeed: %v", probe)
	}
	var modelID string
	for _, item := range h.want(b, "GET", path+"/models", nil, nil, 200)["items"].([]any) {
		m := item.(map[string]any)
		if m["upstream_model"] == vendorModel {
			modelID = m["id"].(string)
		}
	}
	detail := h.want(b, "GET", path, nil, nil, 200)
	detail = h.want(b, "PATCH", path+"/models/"+modelID, map[string]any{
		"enabled": true,
		"capabilities": []any{
			map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
			map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"},
		},
	}, etagHeader(detail), 200)
	certified := h.want(b, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		h.t.Fatalf("certification must succeed: %v", certified)
	}
	detail = h.want(b, "GET", path, nil, nil, 200)
	h.want(b, "POST", path+"/activate", nil, withMatch(detail, idem("provider-activate-"+uuid.NewString())), 200)
}

func login(h *accessHarness, email string) *browser {
	h.t.Helper()
	b := &browser{}
	h.want(b, "POST", "/api/v1/sessions", map[string]any{"email": email, "password": accessPassword}, nil, 201)
	return b
}

func usageMuxFor(h *accessHarness) *httptest.Server {
	h.t.Helper()
	mux := http.NewServeMux()
	management.Register(mux)
	h.Server.Register(mux)
	(&usage.Server{Access: h.Server, VendorKind: providers.VendorKind}).Register(mux)
	server := httptest.NewServer(mux)
	h.t.Cleanup(server.Close)
	return server
}

func (h *accessHarness) browserOn(base string, b *browser, method, path string, body any, headers map[string]string) (int, map[string]any) {
	h.t.Helper()
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			h.t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, base+path, bytes.NewReader(data))
	if err != nil {
		h.t.Fatal(err)
	}
	r.Header.Set("Origin", h.Server.Origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "Chrome test-private-agent")
	for _, c := range b.Cookies {
		r.AddCookie(c)
	}
	r.Header.Set("X-CSRF-Token", b.CSRF)
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			h.t.Fatal("invalid JSON response", err)
		}
	}
	return response.StatusCode, out
}

func TestResourceScopeAssignedMembers(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()

	projectA := createProject(h, owner, "Alpha")
	projectB := createProject(h, owner, "Beta")
	if status, out, _ := h.request(owner, "POST", "/api/v1/projects", map[string]any{"name": " alpha "}, idem("project-dup")); status != 409 || problemCode(t, out) != "project_name_taken" {
		t.Fatal("duplicate project names must conflict", status, out)
	}

	op := h.invite(owner, "op@example.com", "operator")
	op2 := h.invite(owner, "op2@example.com", "operator")
	users := h.want(owner, "GET", "/api/v1/users", nil, nil, 200)
	ids := map[string]map[string]any{}
	for _, item := range users["items"].([]any) {
		record := item.(map[string]any)
		ids[record["email"].(string)] = record
	}
	opID, op2ID := ids["op@example.com"]["id"].(string), ids["op2@example.com"]["id"].(string)

	h.want(op, "GET", "/api/v1/projects", nil, nil, 403)
	h.want(op, "POST", "/api/v1/projects", map[string]any{"name": "Denied"}, idem("project-denied"), 403)

	ownerMemberships := h.want(owner, "GET", "/api/v1/project-memberships", nil, nil, 200)
	if len(ownerMemberships["items"].([]any)) != 2 {
		t.Fatal("the owner must manage every project", ownerMemberships)
	}

	h.want(owner, "PATCH", "/api/v1/users/"+opID, map[string]any{"access_scope": "assigned"}, etagHeader(ids["op@example.com"]), 200)
	h.want(owner, "PATCH", "/api/v1/users/"+op2ID, map[string]any{"access_scope": "assigned"}, etagHeader(ids["op2@example.com"]), 200)
	addMember(h, owner, projectA, opID, "manager")
	addMember(h, owner, projectA, op2ID, "viewer")

	ownerID := ids["owner@example.com"]["id"].(string)
	if status, out, _ := h.request(owner, "DELETE", "/api/v1/projects/"+projectB+"/members/"+ownerID, nil, projectEtag(h, owner, projectB)); status != 409 || problemCode(t, out) != "last_project_manager" {
		t.Fatal("removing the last project manager must conflict", status, out)
	}

	op = login(h, "op@example.com")
	op2 = login(h, "op2@example.com")

	memberships := h.want(op, "GET", "/api/v1/project-memberships", nil, nil, 200)
	items := memberships["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != projectA || items[0].(map[string]any)["role"] != "manager" {
		t.Fatal("the assigned operator must see only project A", memberships)
	}
	for _, path := range []string{"/api/v1/users", "/api/v1/settings", "/api/v1/audit", "/api/v1/management-tokens"} {
		h.want(op, "GET", path, nil, nil, 403)
	}

	up := newVendor(t)
	globalProvider := createScopedProvider(h, owner, "Global vendor", up.URL+"/v1", nil, 201)
	providerB := createScopedProvider(h, owner, "Beta vendor", up.URL+"/v1", projectB, 201)
	providerA := createScopedProvider(h, op, "Alpha vendor", up.URL+"/v1", projectA, 201)
	activateScopedProvider(h, owner, providerB)
	activateScopedProvider(h, op, providerA)
	providerAID := providerA["id"].(string)
	if providerA["project_id"] != projectA {
		t.Fatal("the created provider must expose its project", providerA)
	}
	if detail := h.want(op, "GET", "/api/v1/providers/"+providerAID, nil, nil, 200); detail["project_name"] != "Alpha" {
		t.Fatal("provider detail must expose the project name", detail)
	}
	h.want(op, "POST", "/api/v1/providers", map[string]any{
		"name": "Unscoped", "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": "http://127.0.0.1:9/v1/"},
		"credential": "x",
	}, idem("provider-unscoped"), 403)
	h.want(op, "POST", "/api/v1/providers", map[string]any{
		"name": "Foreign", "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": "http://127.0.0.1:9/v1/"},
		"credential": "x", "project_id": projectB,
	}, idem("provider-foreign"), 404)
	h.want(op, "POST", "/api/v1/providers", map[string]any{
		"name": "Missing", "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": "http://127.0.0.1:9/v1/"},
		"credential": "x", "project_id": uuid.NewString(),
	}, idem("provider-missing"), 404)

	listed := h.want(op, "GET", "/api/v1/providers", nil, nil, 200)
	providers := listed["items"].([]any)
	if len(providers) != 1 || providers[0].(map[string]any)["id"] != providerAID {
		t.Fatal("the assigned operator must list only project A providers", listed)
	}
	for _, id := range []string{providerB["id"].(string), globalProvider["id"].(string)} {
		h.want(op, "GET", "/api/v1/providers/"+id, nil, nil, 404)
		h.want(op, "GET", "/api/v1/providers/"+id+"/models", nil, nil, 404)
	}
	h.want(op, "GET", "/api/v1/providers/"+providerAID+"/models", nil, nil, 200)
	inventory := h.want(op, "GET", "/api/v1/provider-models", nil, nil, 200)["items"].([]any)
	if len(inventory) == 0 {
		t.Fatal("the model inventory must include the caller's own project")
	}
	for _, item := range inventory {
		if item.(map[string]any)["provider_id"] != providerAID {
			t.Fatal("the model inventory must be narrowed to accessible projects", item)
		}
	}
	if len(h.want(owner, "GET", "/api/v1/provider-models", nil, nil, 200)["items"].([]any)) <= len(inventory) {
		t.Fatal("the global inventory must include every project")
	}
	h.want(op, "GET", "/api/v1/runtime-generations", nil, nil, 403)
	h.want(owner, "GET", "/api/v1/runtime-generations", nil, nil, 200)
	// Scope is decided before a provider's precondition or credential is read.
	h.want(op, "POST", "/api/v1/providers/"+providerB["id"].(string)+"/probe", nil, map[string]string{"If-Match": `"stale"`}, 404)
	h.want(op, "POST", "/api/v1/providers/"+providerB["id"].(string)+"/discovery", map[string]any{}, nil, 404)

	op2Listed := h.want(op2, "GET", "/api/v1/providers", nil, nil, 200)
	if len(op2Listed["items"].([]any)) != 1 {
		t.Fatal("a project viewer must read project providers", op2Listed)
	}
	h.want(op2, "POST", "/api/v1/providers", map[string]any{
		"name": "Viewer denied", "configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": "http://127.0.0.1:9/v1/"},
		"credential": "x", "project_id": projectA,
	}, idem("provider-viewer"), 403)

	draftB := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": "beta-route", "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets":    []any{map[string]any{"provider_id": providerB["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
		"project_id": projectB,
	}, idem("draft-beta"), 201)
	draft := h.want(op, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": "alpha-route", "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets":    []any{map[string]any{"provider_id": providerAID, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
		"project_id": projectA,
	}, idem("draft-alpha"), 201)
	draftID := draft["id"].(string)

	if status, out, _ := h.request(op, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": "cross-target", "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1,
		"targets":    []any{map[string]any{"provider_id": providerB["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
		"project_id": projectA,
	}, idem("draft-cross")); status != 422 || problemCode(t, out) != "target_project_mismatch" {
		t.Fatal("a cross-project draft target must be rejected", status, out)
	}
	drafts := h.want(op, "GET", "/api/v1/route-drafts", nil, nil, 200)
	if len(drafts["items"].([]any)) != 1 || drafts["items"].([]any)[0].(map[string]any)["id"] != draftID {
		t.Fatal("the assigned operator must list only project A drafts", drafts)
	}
	h.want(op, "GET", "/api/v1/route-drafts/"+draftB["id"].(string), nil, nil, 404)
	h.want(op2, "PUT", "/api/v1/route-drafts/"+draftID, map[string]any{
		"slug": "alpha-route", "operations": []string{"generation"}, "overall_timeout_ms": 5000, "max_attempts": 1,
		"targets": []any{map[string]any{"provider_model_id": "00000000-0000-0000-0000-000000000000", "priority": 0, "weight": 1, "timeout_ms": 2000}},
	}, nil, 403)

	activated := h.want(op, "POST", "/api/v1/route-drafts/"+draftID+"/activate", nil, withMatch(draft, idem("activate-alpha")), 200)
	routeAID := activated["route_id"].(string)
	if routeA := h.want(op, "GET", "/api/v1/routes/"+routeAID, nil, nil, 200); routeA["project_id"] != projectA {
		t.Fatal("activation must copy the draft project to the route", routeA)
	}
	routeBActivation := h.want(owner, "POST", "/api/v1/route-drafts/"+draftB["id"].(string)+"/activate", nil, withMatch(draftB, idem("activate-beta")), 200)
	routeB := map[string]any{"id": routeBActivation["route_id"]}
	routes := h.want(op, "GET", "/api/v1/routes", nil, nil, 200)
	if len(routes["items"].([]any)) != 1 || routes["items"].([]any)[0].(map[string]any)["slug"] != "alpha-route" {
		t.Fatal("the assigned operator must list only project A routes", routes)
	}
	h.want(op, "GET", "/api/v1/routes/"+routeB["id"].(string), nil, nil, 404)

	globalKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "global", "scopes": []string{"inference"}}, idem("key-global"), 201)
	keyA := h.want(op, "POST", "/api/v1/api-keys", map[string]any{
		"name": "alpha", "scopes": []string{"inference"}, "project_id": projectA,
		"allowed_routes": []string{"alpha-route"},
	}, idem("key-alpha"), 201)
	keyAID := keyA["id"].(string)
	h.want(op, "POST", "/api/v1/api-keys", map[string]any{"name": "foreign", "scopes": []string{"inference"}, "project_id": projectB}, idem("key-foreign"), 404)
	if status, out, _ := h.request(op, "POST", "/api/v1/api-keys", map[string]any{
		"name": "cross", "scopes": []string{"inference"}, "project_id": projectA, "allowed_routes": []string{"beta-route"},
	}, idem("key-cross")); status != 422 {
		t.Fatal("a cross-project route allowlist must be rejected", status, out)
	}
	if status, _, _ := h.request(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "global-cross", "scopes": []string{"inference"}, "allowed_routes": []string{"alpha-route"},
	}, idem("key-global-cross")); status != 422 {
		t.Fatal("a global key must not allowlist a project route", status)
	}
	keys := h.want(op, "GET", "/api/v1/api-keys", nil, nil, 200)
	if len(keys["items"].([]any)) != 1 || keys["items"].([]any)[0].(map[string]any)["id"] != keyAID {
		t.Fatal("the assigned operator must list only project A keys", keys)
	}
	h.want(op, "GET", "/api/v1/api-keys/"+globalKey["id"].(string), nil, nil, 404)

	overview := h.want(op, "GET", "/api/v1/overview", nil, nil, 200)
	if overview["active_routes"].(float64) != 1 || overview["usable_api_key"] != true {
		t.Fatal("overview counts must be narrowed to accessible projects", overview)
	}
	ownerOverview := h.want(owner, "GET", "/api/v1/overview", nil, nil, 200)
	if ownerOverview["active_routes"].(float64) < 2 {
		t.Fatal("the global overview must count every project", ownerOverview)
	}
	health := h.want(op, "GET", "/api/v1/provider-health", nil, nil, 200)
	for _, item := range health["items"].([]any) {
		if item.(map[string]any)["provider_id"] != providerAID {
			t.Fatal("provider health must be narrowed to accessible projects", health)
		}
	}

	usageHTTP := usageMuxFor(h)
	if status, _ := h.browserOn(usageHTTP.URL, op, "GET", "/api/v1/usage/summary?start=2024-01-01T00:00:00Z&end=2024-01-02T00:00:00Z", nil, nil); status != 200 {
		t.Fatal("an assigned user must read scoped usage", status)
	}
	if status, _ := h.browserOn(usageHTTP.URL, op, "POST", "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().UTC().Format(time.RFC3339)}, idem("pricing-denied")); status != 403 {
		t.Fatal("pricing must remain installation-wide", status)
	}
	if status, _ := h.browserOn(usageHTTP.URL, op, "POST", "/api/v1/request-metadata/gateway-epochs/"+uuid.NewString()+"/acknowledge", nil, nil); status != 403 {
		t.Fatal("gateway epochs must remain installation-wide", status)
	}

	// The playground answers an out-of-scope route exactly like a missing one.
	for _, model := range []string{"beta-route", "missing-route"} {
		if status, out, _ := h.request(op, "POST", "/api/v1/playground", map[string]any{"model": model, "input": "hi"}, nil); status != 404 || problemCode(t, out) != "route_not_found" {
			t.Fatal("the playground must not reveal an out-of-scope route", model, status, out)
		}
	}
}
