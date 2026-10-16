//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOrganizationsDelegateProjectsWithoutChangingInstallationRoles(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	org := h.want(owner, "POST", "/api/v1/organizations", map[string]any{"name": "North"}, idem("org"), 201)
	oid := org["id"].(string)
	base := "/api/v1/organizations/" + oid
	manager := h.invite(owner, "org-manager@example.com", "developer")
	u := h.want(manager, "GET", "/api/v1/profile", nil, nil, 200)
	uid := u["id"].(string)
	h.want(owner, "PATCH", "/api/v1/users/"+uid, map[string]any{"access_scope": "assigned"}, etagHeader(u), 200)
	h.want(manager, "POST", "/api/v1/sessions", map[string]any{"email": "org-manager@example.com", "password": accessPassword}, nil, 201)
	h.want(manager, "GET", base, nil, nil, 404)
	h.want(owner, "PUT", base+"/members/"+uuid.NewString(), map[string]any{"role": "manager"}, etagHeader(org), 404)
	h.want(owner, "PUT", base+"/members/"+uid, map[string]any{"role": "manager"}, etagHeader(org), 204)
	first := h.want(owner, "GET", base+"/members?limit=1", nil, nil, 200)
	cursor, _ := first["next_cursor"].(string)
	if len(first["items"].([]any)) != 1 || cursor == "" {
		t.Fatalf("first member page: %v", first)
	}
	last := h.want(owner, "GET", base+"/members?limit=1&cursor="+cursor, nil, nil, 200)
	if len(last["items"].([]any)) != 1 || last["next_cursor"] != nil || last["items"].([]any)[0].(map[string]any)["user_id"] == first["items"].([]any)[0].(map[string]any)["user_id"] {
		t.Fatalf("last member page: %v", last)
	}
	h.want(owner, "GET", base+"/members?cursor=bad", nil, nil, 422)
	project := h.want(manager, "POST", base+"/projects", map[string]any{"name": "North team"}, idem("project"), 201)
	pid := project["id"].(string)
	if project["organization_id"] != oid {
		t.Fatal(project)
	}
	h.want(manager, "GET", "/api/v1/users", nil, nil, 403)
	h.want(manager, "POST", "/api/v1/organizations", map[string]any{"name": "Escalation"}, idem("forbidden"), 403)
	h.want(manager, "PATCH", base+"/projects/"+pid, map[string]any{"name": "North application"}, etagHeader(project), 200)
	h.want(manager, "POST", "/api/v1/api-keys", map[string]any{"name": "org key", "project_id": pid, "scopes": []string{"inference"}, "allowed_routes": []string{}}, idem("key"), 201)
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "org token", "scopes": []string{"read", "keys", "manage_organization"}, "project_ids": []string{pid}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	h.machineWant(token, "GET", base, nil, nil, 404)
	other := h.want(owner, "POST", "/api/v1/organizations", map[string]any{"name": "South"}, idem("other"), 201)["id"].(string)
	h.want(manager, "GET", "/api/v1/organizations/"+other, nil, nil, 404)
	h.want(manager, "GET", "/api/v1/organizations/"+other+"/projects/"+pid, nil, nil, 404)
	org = h.want(owner, "GET", base, nil, nil, 200)
	h.want(owner, "DELETE", base+"/members/"+uid, nil, etagHeader(org), 204)
	h.want(manager, "GET", base, nil, nil, 404)
	h.want(manager, "GET", "/api/v1/projects/"+pid+"/budget", nil, nil, 404)
	memberships := h.want(manager, "GET", "/api/v1/project-memberships", nil, nil, 200)["items"].([]any)
	if len(memberships) != 0 {
		t.Fatalf("organization creator retained access: %v", memberships)
	}
}

// A project the document creates in an organization is managed through it,
// with no direct grant to whoever applied the document.

// Global ownership alone does not let a token without access manage unrelated organizations.

// Managing the organization is enough to raise its budget for a while.
