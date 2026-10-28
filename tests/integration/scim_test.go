//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/scim"
	"github.com/tyk-swe/olp/sdk/management"
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

// The SCIM grant satisfies the console's rule, so the direct manager leaves.

// Taking the user over locally would end their group grant too.

// The console's mapping editor applies the same rule.

// Another delegated manager lets the directory withdraw its grant.

// Directory replacement preserves an unfamiliar omitted extension.

// Model an independently enrolled local sign-in; SCIM never accepts passwords.

func TestSCIMSDKConditionalWriteRoundTrip(t *testing.T) {
	h := newAccessHarness(t)
	token := scimToken(h, h.owner(), []string{"access"})
	user := scimCall(t, h, token, "POST", "/scim/v2/Users", scimUser("etag-roundtrip"), "", 201)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := management.NewClient(h.HTTP.URL, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	get, _ := management.Lookup("scim_users_get")
	patch, _ := management.Lookup("scim_users_patch")
	path := map[string]string{"scim_id": user["id"].(string)}
	response, err := client.Call(t.Context(), get, management.Arguments{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(scimPatch(map[string]any{"op": "replace", "path": "displayName", "value": "Changed"}))
	if err != nil {
		t.Fatal(err)
	}
	args := management.Arguments{Path: path, IfMatch: response.ETag, Body: body}
	updated, err := client.Call(t.Context(), patch, args)
	if err != nil {
		t.Fatalf("unchanged SCIM version refused: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(updated.Body, &document); err != nil {
		t.Fatal(err)
	}
	if document["displayName"] != "Changed" {
		t.Fatalf("patch did not update resource: %s", updated.Body)
	}
	_, stale := client.Call(t.Context(), patch, args)
	var problem *management.APIError
	if !errors.As(stale, &problem) || problem.Status != http.StatusPreconditionFailed {
		t.Fatalf("stale version accepted: %v", stale)
	}
}
