//go:build integration

package integration_test

import (
	"github.com/tyk-swe/olp/internal/gateway"
	"log/slog"
	"testing"
	"time"
)

func TestLimitTemplatesPropagateToKeysUsersAndGroups(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Templates"}, idem("template-project"), 201)
	id := project["id"].(string)
	path := "/api/v1/projects/" + id + "/limit-templates"
	templates := map[string]any{"key": map[string]any{"requests_per_minute": 2}, "customer": map[string]any{"requests_per_minute": 1}, "group": map[string]any{"requests_per_minute": 2}}
	put := func(want int) {
		t.Helper()
		old := h.want(owner, "GET", path, nil, nil, 200)
		h.want(owner, "PUT", path, map[string]any{"templates": templates}, etagHeader(old), want)
	}
	put(200)
	fixture := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Template provider", fixture.URL, &id, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	slug := "template-chat"
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": id, "slug": slug, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("template-route"), 201)
	dp := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", dp+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", dp+"/activate", nil, withMatch(validated, idem("activate-template-route")), 200)
	client := limClient(t)
	limiter := limLimiter(t, client, limNamespace(t, client, "templates"))
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	replica.Gateway.Admission = h.Gateway.Admission
	key := func(name string, extra map[string]any) string {
		t.Helper()
		in := map[string]any{"name": name, "project_id": id, "scopes": []string{"inference"}, "allowed_routes": []string{slug}}
		for k, v := range extra {
			in[k] = v
		}
		made := h.want(owner, "POST", "/api/v1/api-keys", in, idem(name), 201)
		h.refresh()
		replica.refresh()
		return made["secret"].(string)
	}
	call := func(server *accessHarness, secret, user string, want int) {
		t.Helper()
		status, err := endUserChat(t.Context(), server, secret, slug, user)
		if err != nil || status != want {
			t.Fatalf("status=%d want=%d err=%v", status, want, err)
		}
	}
	a := key("key-a", map[string]any{"limit_template": "key", "requests_per_minute": 100})
	b := key("key-b", map[string]any{"limit_template": "key"})
	limSettleInMinute(t, client, 10*time.Second)
	call(h, a, "", 200)
	call(replica, a, "", 200)
	call(h, a, "", 429)
	call(h, b, "", 200)
	templates["key"] = map[string]any{"requests_per_minute": 3}
	put(200)
	h.refresh()
	replica.refresh()
	call(replica, a, "", 200)
	call(h, a, "", 429)
	identified := key("identified", map[string]any{"end_user_source": "native", "end_user_policy": map[string]any{"limit_template": "customer"}})
	call(h, identified, "alice", 200)
	call(replica, identified, "alice", 429)
	call(h, identified, "bob", 200)
	group := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "Template group", "project_id": id, "limit_template": "group"}, idem("template-group"), 201)
	c := key("group-c", map[string]any{"budget_group_id": group["id"]})
	d := key("group-d", map[string]any{"budget_group_id": group["id"]})
	call(h, c, "", 200)
	call(replica, d, "", 200)
	call(h, c, "", 429)
	templates["group"] = map[string]any{"requests_per_minute": 3, "daily_cost_limit": "4"}
	put(200)
	h.refresh()
	replica.refresh()
	detail := h.want(owner, "GET", "/api/v1/budget-groups/"+group["id"].(string), nil, nil, 200)
	if detail["budget"].(map[string]any)["daily"].(map[string]any)["limit"] != "4" {
		t.Fatalf("effective group reporting=%v", detail)
	}
	delete(templates, "key")
	put(409)
	// Unknown and cross-project references cannot be saved.
	h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Bad template", "scopes": []string{"inference"}, "allowed_routes": []string{}, "limit_template": "key"}, idem("unassigned-template"), 422)
}

func TestLimitTemplatesPromoteWithReferencesAndPermissions(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Portable templates"}, idem("project"), 201)
	id := project["id"].(string)
	path := "/api/v1/projects/" + id + "/limit-templates"
	current := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{"templates": map[string]any{"standard": map[string]any{"requests_per_minute": 2}}}, etagHeader(current), 200)
	h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Member", "project_id": id, "scopes": []string{"inference"}, "allowed_routes": []string{}, "limit_template": "standard"}, idem("member"), 201)
	ep := "/api/v1/projects/" + id + "/end-user-policy"
	current = h.want(owner, "GET", ep, nil, nil, 200)
	h.want(owner, "PUT", ep, map[string]any{"policy": map[string]any{"limit_template": "standard"}}, etagHeader(current), 200)
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	entry := doc["projects"].([]any)[0].(map[string]any)
	if entry["end_user_limit_template"] != "standard" {
		t.Fatal("project reference missing from export")
	}
	entry["limit_templates"] = map[string]any{"standard": map[string]any{"requests_per_minute": 5}}
	body := map[string]any{"document": doc}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "Configure only", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("denied"), 403)
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("apply"), 200)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("unchanged"), 200)
	entry["limit_templates"] = map[string]any{}
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("referenced-delete"), 409)
	saved := h.want(owner, "GET", path, nil, nil, 200)["templates"].(map[string]any)["standard"].(map[string]any)
	if saved["requests_per_minute"] != float64(5) {
		t.Fatalf("failed removal changed policy: %v", saved)
	}
}
