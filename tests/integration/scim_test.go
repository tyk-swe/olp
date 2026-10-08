//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/scim"
	"github.com/tyk-swe/olp/internal/secrets"
)

func scimToken(h *accessHarness, owner *browser, scopes []string) string {
	h.t.Helper()
	return h.want(owner, "POST", "/api/v1/management-tokens", map[string]any{"name": "SCIM", "scopes": scopes, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}, idem("scim-token-"+strings.Join(scopes, "-")), 201)["secret"].(string)
}
func scimCall(t *testing.T, h *accessHarness, token, method, path string, body any, etag string, want int) map[string]any {
	t.Helper()
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	r, e := http.NewRequest(method, h.HTTP.URL+path, bytes.NewReader(data))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/scim+json")
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	response, e := h.HTTP.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, response.StatusCode, want, raw)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/scim+json") {
		t.Fatalf("wrong SCIM media type: %s", response.Header.Get("Content-Type"))
	}
	out := map[string]any{}
	if len(raw) > 0 && json.Unmarshal(raw, &out) != nil {
		t.Fatalf("invalid JSON: %s", raw)
	}
	if want >= 400 {
		if out["status"] != fmt.Sprint(want) {
			t.Fatalf("SCIM error missing string status: %s", raw)
		}
	}
	return out
}
func scimUser(name string) map[string]any {
	return map[string]any{"schemas": []string{scim.UserSchema}, "externalId": "external-" + name, "userName": name + "@example.com", "displayName": name, "active": true, "name": map[string]any{"givenName": name, "familyName": "Example"}, "emails": []any{map[string]any{"value": name + "@example.com", "type": "work", "primary": true}}}
}
func scimPatch(operations ...any) map[string]any {
	return map[string]any{"schemas": []string{scim.PatchSchema}, "Operations": operations}
}
func TestSCIMUsersGroupsAndInheritedGrants(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	token := scimToken(h, owner, []string{"access"})
	scimCall(t, h, scimToken(h, owner, []string{"read"}), "GET", "/scim/v2/Users", nil, "", 403)
	config := scimCall(t, h, token, "GET", "/scim/v2/ServiceProviderConfig", nil, "", 200)
	if config["patch"].(map[string]any)["supported"] != true {
		t.Fatal(config)
	}
	for _, path := range []string{"/scim/v2/Schemas", "/scim/v2/ResourceTypes"} {
		scimCall(t, h, token, "GET", path, nil, "", 200)
	}
	user := scimCall(t, h, token, "POST", "/scim/v2/Users", scimUser("alice"), "", 201)
	uid := user["id"].(string)
	upath := "/scim/v2/Users/" + uid
	scimCall(t, h, token, "POST", "/scim/v2/Users", scimUser("alice"), "", 409)
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "SCIM project"}, idem("scim-project"), 201)["id"].(string)
	group := scimCall(t, h, token, "POST", "/scim/v2/Groups", map[string]any{"schemas": []string{scim.GroupSchema}, "displayName": "Developers", "externalId": "developers", "members": []any{map[string]any{"value": uid}}, scim.GroupExtension: map[string]any{"role": "developer", "projects": []any{map[string]any{"value": project, "role": "manager"}}}}, "", 201)
	gpath := "/scim/v2/Groups/" + group["id"].(string)
	scimCall(t, h, token, "PATCH", upath, scimPatch(map[string]any{"op": "replace", "path": "displayName", "value": "Alice Updated"}), "", 200)
	changedGroup := scimCall(t, h, token, "GET", gpath, nil, "", 200)
	if changedGroup["meta"].(map[string]any)["version"] == group["meta"].(map[string]any)["version"] {
		t.Fatal("computed member display changed without a group ETag change")
	}
	scimCall(t, h, token, "PATCH", upath, scimPatch(map[string]any{"op": "replace", "path": "displayName", "value": "alice"}), "", 200)

	var role, scope, projectRole string
	if e := h.Pool.QueryRow(t.Context(), "SELECT u.role,u.access_scope,m.role FROM olp.users u JOIN olp.effective_project_members m ON m.user_id=u.id WHERE u.id=$1 AND m.project_id=$2", uid, project).Scan(&role, &scope, &projectRole); e != nil || role != "developer" || scope != "assigned" || projectRole != "manager" {
		t.Fatalf("role=%s scope=%s project=%s err=%v", role, scope, projectRole, e)
	}
	for _, filter := range []string{`userName eq "ALICE@example.com"`, `emails[type eq "work" and value co "@example.com"]`, `name.givenName sw "ali" and not (active eq false)`, `externalId pr and userName ew "example.com"`, `meta.created le "2100-01-01T00:00:00Z"`} {
		listed := scimCall(t, h, token, "GET", "/scim/v2/Users?filter="+url.QueryEscape(filter), nil, "", 200)
		if listed["totalResults"] != float64(1) {
			t.Fatalf("filter %s: %v", filter, listed)
		}
	}
	listed := scimCall(t, h, token, "POST", "/scim/v2/Users/.search", map[string]any{"schemas": []string{scim.SearchSchema}, "count": 0, "filter": `active eq true`}, "", 200)
	if listed["totalResults"] != float64(1) || listed["itemsPerPage"] != float64(0) {
		t.Fatal(listed)
	}
	scimCall(t, h, token, "GET", "/scim/v2/Users?filter="+url.QueryEscape(`userName eq "x' OR TRUE --"`), nil, "", 200)
	scimCall(t, h, token, "GET", "/scim/v2/Users?filter="+url.QueryEscape(`active gt true`), nil, "", 400)
	old := user["meta"].(map[string]any)["version"].(string)
	changed := scimCall(t, h, token, "PATCH", upath, scimPatch(map[string]any{"op": "replace", "path": "name.familyName", "value": "Updated"}, map[string]any{"op": "replace", "path": `emails[type eq "work"].value`, "value": "new@example.com"}), "", 200)
	if changed["name"].(map[string]any)["familyName"] != "Updated" {
		t.Fatal(changed)
	}
	scimCall(t, h, token, "PATCH", upath, scimPatch(map[string]any{"op": "replace", "path": "active", "value": false}), old, 412)
	scimCall(t, h, token, "PATCH", upath, scimPatch(map[string]any{"op": "replace", "path": "displayName", "value": "Must rollback"}, map[string]any{"op": "remove", "path": "id"}), "", 400)
	if current := scimCall(t, h, token, "GET", upath, nil, "", 200); current["displayName"] != "alice" {
		t.Fatal("PATCH was not atomic")
	}
	// A direct grant is independent of the inherited group grant.
	p := h.want(owner, "GET", "/api/v1/projects/"+project, nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/projects/"+project+"/members/"+uid, map[string]any{"role": "viewer"}, etagHeader(p), 200)
	scimCall(t, h, token, "PATCH", gpath, scimPatch(map[string]any{"op": "remove", "path": `members[value eq "` + uid + `"]`}), "", 200)
	if e := h.Pool.QueryRow(t.Context(), "SELECT u.role,m.role FROM olp.users u JOIN olp.effective_project_members m ON m.user_id=u.id WHERE u.id=$1 AND m.project_id=$2", uid, project).Scan(&role, &projectRole); e != nil || role != "viewer" || projectRole != "viewer" {
		t.Fatalf("direct grant lost: %s/%s %v", role, projectRole, e)
	}
	scimCall(t, h, token, "DELETE", upath, nil, "", 204)
	scimCall(t, h, token, "GET", upath, nil, "", 404)
	restored := scimCall(t, h, token, "POST", "/scim/v2/Users", scimUser("alice"), "", 201)
	if restored["id"] != uid {
		t.Fatal("re-enrollment changed the identity")
	}
	// The externalId also restores an identity whose userName changed meanwhile,
	// but not onto an address another account holds.
	scimCall(t, h, token, "DELETE", upath, nil, "", 204)
	renamed := scimUser("alice")
	renamed["userName"] = "owner@example.com"
	scimCall(t, h, token, "POST", "/scim/v2/Users", renamed, "", 409)
	renamed["userName"] = "alice.renamed@example.com"
	if restored = scimCall(t, h, token, "POST", "/scim/v2/Users", renamed, "", 201); restored["id"] != uid || restored["userName"] != "alice.renamed@example.com" {
		t.Fatalf("renamed re-enrollment: %v", restored)
	}
	// A local owner takeover cannot be undone through SCIM reconciliation.
	local := h.want(owner, "GET", "/api/v1/users/"+uid, nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/users/"+uid, map[string]any{"role": "developer"}, etagHeader(local), 200)
	scimCall(t, h, token, "PATCH", upath, scimPatch(map[string]any{"op": "replace", "path": "active", "value": false}), "", 409)
	scimCall(t, h, token, "DELETE", gpath, nil, "", 204)
}

func TestSCIMKeepsAProjectManager(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	token := scimToken(h, owner, []string{"access"})
	uid := scimCall(t, h, token, "POST", "/scim/v2/Users", scimUser("manager"), "", 201)["id"].(string)
	upath := "/scim/v2/Users/" + uid
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "SCIM managed"}, idem("scim-managed-project"), 201)["id"].(string)
	group := scimCall(t, h, token, "POST", "/scim/v2/Groups", map[string]any{"schemas": []string{scim.GroupSchema}, "displayName": "Managers", "members": []any{map[string]any{"value": uid}}, scim.GroupExtension: map[string]any{"role": "developer", "projects": []any{map[string]any{"value": project, "role": "manager"}}}}, "", 201)
	gpath := "/scim/v2/Groups/" + group["id"].(string)
	// The SCIM grant satisfies the console's rule, so the direct manager leaves.
	ownerID := h.want(owner, "GET", "/api/v1/profile", nil, nil, 200)["id"].(string)
	h.want(owner, "DELETE", "/api/v1/projects/"+project+"/members/"+ownerID, nil, projectEtag(h, owner, project), 204)

	scimCall(t, h, token, "PATCH", gpath, scimPatch(map[string]any{"op": "remove", "path": `members[value eq "` + uid + `"]`}), "", 409)
	scimCall(t, h, token, "PATCH", gpath, scimPatch(map[string]any{"op": "replace", "path": scim.GroupExtension + ":projects", "value": []any{map[string]any{"value": project, "role": "viewer"}}}), "", 409)
	scimCall(t, h, token, "DELETE", gpath, nil, "", 409)
	scimCall(t, h, token, "DELETE", upath, nil, "", 409)
	// Taking the user over locally would end their group grant too.
	provisioned := h.want(owner, "GET", "/api/v1/users/"+uid, nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/users/"+uid, map[string]any{"role": "developer"}, etagHeader(provisioned), 409)
	// The console's mapping editor applies the same rule.
	mapping := h.want(owner, "GET", "/api/v1/scim/groups", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	h.want(owner, "PUT", "/api/v1/scim/groups/"+group["id"].(string)+"/mapping", map[string]any{"mapping": map[string]any{"role": "developer", "accessScope": "assigned", "projects": []any{}}}, etagHeader(mapping), 409)
	var managers int
	if e := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.effective_project_members WHERE project_id=$1 AND role='manager'", project).Scan(&managers); e != nil || managers != 1 {
		t.Fatalf("managers=%d err=%v", managers, e)
	}

	// Another delegated manager lets the directory withdraw its grant.
	p := h.want(owner, "GET", "/api/v1/projects/"+project, nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/projects/"+project+"/members/"+ownerID, map[string]any{"role": "manager"}, etagHeader(p), 200)
	scimCall(t, h, token, "DELETE", gpath, nil, "", 204)
}

func TestSCIMMappingsPromoteWithoutIdentitiesOrMemberships(t *testing.T) {
	source := newAccessHarness(t)
	owner := source.owner()
	token := scimToken(source, owner, []string{"access"})
	project := source.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Build"}, idem("scim-promotion-project"), 201)["id"].(string)
	user := scimCall(t, source, token, "POST", "/scim/v2/Users", scimUser("private-directory-subject"), "", 201)
	group := scimCall(t, source, token, "POST", "/scim/v2/Groups", map[string]any{"schemas": []string{scim.GroupSchema}, "displayName": "Builders", "externalId": "private-directory-group", "members": []any{map[string]any{"value": user["id"]}}}, "", 201)
	managed := source.want(owner, "GET", "/api/v1/scim/groups", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	source.want(owner, "PUT", "/api/v1/scim/groups/"+group["id"].(string)+"/mapping", map[string]any{"mapping": map[string]any{"role": "developer", "accessScope": "assigned", "projects": []any{map[string]any{"value": project, "role": "manager"}}}}, etagHeader(managed), 200)
	// Directory replacement preserves an unfamiliar omitted extension.
	scimCall(t, source, token, "PUT", "/scim/v2/Groups/"+group["id"].(string), map[string]any{"schemas": []string{scim.GroupSchema}, "displayName": "Builders", "members": []any{map[string]any{"value": user["id"]}}}, "", 200)
	document := source.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	raw, _ := json.Marshal(document)
	for _, value := range []string{project, user["id"].(string), group["id"].(string), "private-directory-subject", "private-directory-group"} {
		if bytes.Contains(raw, []byte(value)) {
			t.Fatalf("nonportable SCIM identity exported: %s", value)
		}
	}
	destination := newAccessHarness(t)
	targetOwner := destination.owner()
	configure := scimToken(destination, targetOwner, []string{"configure"})
	request := map[string]any{"document": document}
	destination.machineWant(configure, "POST", "/api/v1/configuration/plan", request, nil, 403)
	destination.machineWant(configure, "POST", "/api/v1/configuration/apply", request, idem("scim-import"), 403)
	destination.want(targetOwner, "POST", "/api/v1/configuration/plan", request, nil, 200)
	destination.want(targetOwner, "POST", "/api/v1/configuration/apply", request, idem("scim-import"), 200)
	imported := destination.want(targetOwner, "GET", "/api/v1/scim/groups", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if imported["member_count"] != float64(0) || imported["mapping"].(map[string]any)["role"] != "developer" {
		t.Fatal(imported)
	}
	destination.machineWant(configure, "GET", "/api/v1/configuration/export", nil, nil, 403)
	destination.want(targetOwner, "POST", "/api/v1/configuration/apply", request, idem("scim-import-again"), 200)
	var users int
	if err := destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.scim_users").Scan(&users); err != nil || users != 0 {
		t.Fatalf("users promoted: %d %v", users, err)
	}
}

func TestSCIMRevokesSessionsAndProtectsTheUsableOwner(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	token := scimToken(h, owner, []string{"access"})
	input := scimUser("directory-owner")
	input[scim.UserExtension] = map[string]any{"role": "owner", "accessScope": "global"}
	user := scimCall(t, h, token, "POST", "/scim/v2/Users", input, "", 201)
	id := user["id"].(string)
	// Model an independently enrolled local sign-in; SCIM never accepts passwords.
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.users SET password_hash=$2 WHERE id=$1", id, secrets.HashPassword(accessPassword)); err != nil {
		t.Fatal(err)
	}
	signIn := func() *browser {
		b := &browser{}
		h.want(b, "POST", "/api/v1/sessions", map[string]any{"email": "directory-owner@example.com", "password": accessPassword}, nil, 201)
		return b
	}
	member := signIn()
	scimCall(t, h, token, "PATCH", "/scim/v2/Users/"+id, scimPatch(map[string]any{"op": "replace", "path": "displayName", "value": "Directory Owner"}), "", 200)
	h.want(member, "GET", "/api/v1/profile", nil, nil, 401)
	member = signIn()
	memberToken := scimToken(h, member, []string{"access"})
	profile := h.want(owner, "GET", "/api/v1/profile", nil, nil, 200)
	ownerID := profile["id"].(string)
	detail := h.want(member, "GET", "/api/v1/users/"+ownerID, nil, nil, 200)
	h.want(member, "PATCH", "/api/v1/users/"+ownerID, map[string]any{"active": false}, etagHeader(detail), 200)
	scimCall(t, h, memberToken, "DELETE", "/scim/v2/Users/"+id, nil, "", 409)
	current := scimCall(t, h, memberToken, "GET", "/scim/v2/Users/"+id, nil, "", 200)
	if current["active"] != true {
		t.Fatal("owner refusal did not roll back")
	}
	h.want(member, "GET", "/api/v1/profile", nil, nil, 200)
}

func TestSCIMSortsPrimaryValuesAndProjectsComplexAttributes(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	token := scimToken(h, owner, []string{"access"})
	a := scimUser("sort-a")
	a["emails"] = []any{map[string]any{"value": "a@example.com", "primary": false}, map[string]any{"value": "z@example.com", "primary": true}}
	b := scimUser("sort-b")
	b["emails"] = []any{map[string]any{"value": "m@example.com", "primary": true}}
	firstUser := scimCall(t, h, token, "POST", "/scim/v2/Users", a, "", 201)
	second := scimCall(t, h, token, "POST", "/scim/v2/Users", b, "", 201)
	list := scimCall(t, h, token, "GET", "/scim/v2/Users?sortBy=emails.value&count=1&attributes=emails.value", nil, "", 200)
	first := list["Resources"].([]any)[0].(map[string]any)
	if list["totalResults"] != float64(2) || first["id"] != second["id"] || first["userName"] != nil || first["emails"].([]any)[0].(map[string]any)["primary"] != nil {
		t.Fatal(list)
	}
	list = scimCall(t, h, token, "GET", "/scim/v2/Users?sortBy=emails.value&sortOrder=descending&startIndex=2", nil, "", 200)
	if list["Resources"].([]any)[0].(map[string]any)["id"] != second["id"] {
		t.Fatal(list)
	}
	updated := scimCall(t, h, token, "PATCH", "/scim/v2/Users/"+firstUser["id"].(string), scimPatch(map[string]any{"op": "replace", "path": `emails[value eq "a@example.com"].primary`, "value": true}), "", 200)
	entries := updated["emails"].([]any)
	if entries[0].(map[string]any)["primary"] != true || entries[1].(map[string]any)["primary"] != false {
		t.Fatal("primary email did not transfer", updated)
	}

}
