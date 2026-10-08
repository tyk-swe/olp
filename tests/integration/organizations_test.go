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

func TestOrganizationsPromoteMembershipAndBudgetsWithExplicitAuthority(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	org := h.want(owner, "POST", "/api/v1/organizations", map[string]any{"name": "Promoted org"}, idem("org"), 201)
	base := "/api/v1/organizations/" + org["id"].(string)
	project := h.want(owner, "POST", base+"/projects", map[string]any{"name": "Promoted team"}, idem("project"), 201)
	detail := h.want(owner, "GET", base+"/budget", nil, nil, 200)
	h.want(owner, "PUT", base+"/budget", map[string]any{"policy": map[string]any{"weekly_cost_limit": "10"}}, etagHeader(detail), 200)
	document := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	if document["projects"].([]any)[0].(map[string]any)["organization"] != "Promoted org" {
		t.Fatal("missing organization")
	}
	document["organizations"].([]any)[0].(map[string]any)["budget"] = map[string]any{"weekly_cost_limit": "20"}
	body := map[string]any{"document": document}
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "configure only", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem("config-token"), 201)["secret"].(string)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("denied"), 403)
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("apply"), 200)
	h.machineWant(token, "POST", "/api/v1/configuration/apply", body, idem("noop"), 200)
	increase := h.want(owner, "POST", "/api/v1/budget-increases", map[string]any{"target": map[string]any{"kind": "organization", "id": org["id"]}, "window": "week", "amount": "2", "reason": "Batch"}, idem("increase"), 201)
	h.want(owner, "GET", "/api/v1/budget-increases/"+increase["id"].(string), nil, nil, 200)
	other := h.want(owner, "POST", "/api/v1/organizations", map[string]any{"name": "Other"}, idem("other"), 201)
	document["projects"].([]any)[0].(map[string]any)["organization"] = "Other"
	h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 409)
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("move-denied"), 409)
	h.want(owner, "GET", "/api/v1/organizations/"+other["id"].(string)+"/projects/"+project["id"].(string), nil, nil, 404)
}

func TestOrganizationMembershipIntersectionAndLastManager(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	o := h.want(owner, "POST", "/api/v1/organizations", map[string]any{"name": "Scoped"}, idem("org"), 201)
	base := "/api/v1/organizations/" + o["id"].(string)
	ownerProfile := h.want(owner, "GET", "/api/v1/profile", nil, nil, 200)
	h.want(owner, "DELETE", base+"/members/"+ownerProfile["id"].(string), nil, etagHeader(o), 409)
	user := h.invite(owner, "inherited@example.com", "developer")
	profile := h.want(user, "GET", "/api/v1/profile", nil, nil, 200)
	uid := profile["id"].(string)
	token := h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "creator bound", "scopes": []string{"read", "manage_organization"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem("token"), 201)["secret"].(string)
	// Global ownership alone does not let a token without access manage unrelated organizations.
	h.machineWant(token, "GET", base, nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/users/"+uid, map[string]any{"access_scope": "assigned"}, etagHeader(profile), 200)
	user = login(h, "inherited@example.com")
	h.want(owner, "PUT", base+"/members/"+uid, map[string]any{"role": "viewer"}, etagHeader(o), 204)
	p := h.want(owner, "POST", base+"/projects", map[string]any{"name": "Inherited"}, idem("project"), 201)
	pid := p["id"].(string)
	h.want(user, "GET", "/api/v1/projects/"+pid+"/budget", nil, nil, 200)
	h.want(user, "POST", base+"/projects", map[string]any{"name": "denied"}, idem("deny"), 403)
	h.want(owner, "PUT", base+"/projects/"+pid+"/members/"+uid, map[string]any{"role": "manager"}, etagHeader(p), 200)
	o = h.want(owner, "GET", base, nil, nil, 200)
	h.want(owner, "DELETE", base+"/members/"+uid, nil, etagHeader(o), 204)
	h.want(user, "GET", base, nil, nil, 404)
	detail := h.want(user, "GET", "/api/v1/projects/"+pid+"/budget", nil, nil, 200)
	h.want(user, "PUT", "/api/v1/projects/"+pid+"/budget", map[string]any{"policy": map[string]any{"daily_cost_limit": "1"}}, etagHeader(detail), 200)
}
