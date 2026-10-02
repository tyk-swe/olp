//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

// This fixture proves public management against a fictional grant provider.
// It does not mark that provider as a qualified Codex subscription account.
type codePublicFixture struct {
	h                   *accessHarness
	owner               *browser
	project, keyID, key string
	accounts            []map[string]any
	pool, route         map[string]any
	upstream            *pluginUpstream
	authority           *testutil.OAuthServer
	peer                *codexfixture.Upstream
}

func newCodePublicFixture(t *testing.T) *codePublicFixture {
	return newCodePublicFixtureWithPeer(t, nil)
}

func newCodePublicFixtureWithPeer(t *testing.T, peer *codexfixture.Upstream) *codePublicFixture {
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	f := &codePublicFixture{h: h, owner: owner, project: createProject(h, owner, "Code qualification"), peer: peer}
	f.authority = testutil.NewOAuthServer(t)
	f.upstream = newGrantUpstream(t, f.authority)
	if peer != nil {
		f.upstream.Config.Handler = peer
	}
	digest := installGrantPlugin(t, h, owner, f.upstream, "0.1.0")
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Code key", "project_id": f.project, "scopes": []string{"inference"}, "allowed_routes": []string{"qualification"}}, idem("code-key"), 201)
	f.keyID, f.key = key["id"].(string), key["secret"].(string)
	for i := range 2 {
		f.authority.SignInAs(testutil.OAuthIdentity{Subject: fmt.Sprintf("controlled-%d@example.test", i), Account: fmt.Sprintf("controlled-%d", i)})
		provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{
			"name": fmt.Sprintf("Controlled code account %d", i), "project_id": f.project, "model": vendorModel,
			"configuration": map[string]any{"kind": "plugin", "auth_mode": "grant", "profile_id": "reference-grant-chat", "profile_revision": digest},
		}, idem(fmt.Sprintf("code-provider-%d", i)), 201)
		path := "/api/v1/providers/" + provider["id"].(string)
		enrollment := startGrantEnrollment(t, h, owner, path)
		completed := continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 201)
		account := h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{
			"project_id": f.project, "provider_id": provider["id"], "credential_id": completed["credential_id"], "name": "Controlled fixture only", "enabled": true, "models": []string{"gpt-5.4"},
		}, idem(fmt.Sprintf("code-account-%d", i)), 201)
		f.accounts = append(f.accounts, account)
	}
	f.pool = h.want(owner, "POST", "/api/v1/code/pools", f.poolInput([]string{f.keyID}), idem("code-pool"), 201)
	draft := h.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"gpt-5.4"}, "enabled": true}, idem("code-route"), 201)
	f.route = h.want(owner, "POST", "/api/v1/code/routes/"+draft["id"].(string)+"/publish", nil, withMatch(draft, idem("code-publish")), 200)
	f.noSyntheticInference(t)
	return f
}

func (f *codePublicFixture) noSyntheticInference(t *testing.T) {
	t.Helper()
	if f.peer != nil && len(f.peer.Requests()) != 0 {
		t.Fatal("lifecycle generated traffic to controlled peer")
	}
	if n := len(f.upstream.receivedPaths()); n != 0 {
		t.Fatalf("lifecycle generated %d upstream requests before user inference", n)
	}
}

func (f *codePublicFixture) poolInput(keys []string) map[string]any {
	ids := []string{}
	for _, account := range f.accounts {
		ids = append(ids, account["id"].(string))
	}
	return map[string]any{"project_id": f.project, "name": "Controlled shared pool", "kind": "shared", "owner_user_id": nil, "account_ids": ids, "api_key_ids": keys}
}

func (f *codePublicFixture) list(collection string) []any {
	return f.h.want(f.owner, "GET", "/api/v1/code/"+collection+"?project_id="+f.project, nil, nil, 200)["items"].([]any)
}

func TestCodeQualificationPublicManagementLifecycle(t *testing.T) {
	f := newCodePublicFixture(t)
	h, owner := f.h, f.owner
	for _, account := range f.list("accounts") {
		a := account.(map[string]any)
		if a["health"] != "unknown" || a["allowance"] != nil || a["grant_state"] != "current" {
			t.Fatalf("fabricated qualification/allowance: %v", a)
		}
	}
	for _, collection := range []string{"accounts", "pools", "routes", "budgets", "bindings", "attempts", "refusals", "token-windows"} {
		response, body := h.do(owner, "GET", "/api/v1/code/"+collection+"?project_id="+f.project, nil, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", collection, response.StatusCode)
		}
		for _, secret := range append(f.authority.Issued(), f.key) {
			if strings.Contains(string(body), secret) {
				t.Fatalf("%s exposes credential material", collection)
			}
		}
	}
	path := "/api/v1/code/pools/" + f.pool["id"].(string)
	h.want(owner, "PUT", path, f.poolInput([]string{}), nil, 428)
	updated := h.want(owner, "PUT", path, f.poolInput([]string{}), etagHeader(f.pool), 200)
	h.want(owner, "PUT", path, f.poolInput([]string{f.keyID}), etagHeader(f.pool), 412)
	f.pool = h.want(owner, "PUT", path, f.poolInput([]string{f.keyID}), etagHeader(updated), 200)
	viewer := h.invite(owner, "code-viewer@example.test", "viewer")
	h.want(viewer, "PUT", path, f.poolInput([]string{}), etagHeader(f.pool), 403)
	h.want(owner, "PUT", path, f.poolInput([]string{}), withMatch(f.pool, map[string]string{"X-CSRF-Token": "invalid"}), 403)
	budget := map[string]any{"project_id": f.project, "route_id": f.route["id"], "api_key_id": f.keyID, "daily_tokens": 1000, "monthly_tokens": 10000, "enabled": true}
	created := h.want(owner, "POST", "/api/v1/code/budgets", budget, idem("budget"), 201)
	replayed := h.want(owner, "POST", "/api/v1/code/budgets", budget, idem("budget"), 201)
	if created["id"] != replayed["id"] {
		t.Fatal("budget creation replay duplicated the budget")
	}
	if len(f.list("bindings")) != 0 || len(f.list("attempts")) != 0 {
		t.Fatal("management created inference accounting")
	}
	for _, item := range f.list("token-windows") {
		if len(item.(map[string]any)["windows"].([]any)) != 0 {
			t.Fatal("management created token reservations")
		}
	}
	revisions := h.want(owner, "GET", "/api/v1/code/routes/"+f.route["id"].(string)+"/revisions", nil, nil, 200)["items"].([]any)
	if len(revisions) != 1 {
		t.Fatalf("revisions: %d", len(revisions))
	}
	f.noSyntheticInference(t)
}

func TestCodeQualificationPublicProjectAndPersonalPoolIsolation(t *testing.T) {
	f := newCodePublicFixture(t)
	h, owner := f.h, f.owner
	other := createProject(h, owner, "Other project")
	input := f.poolInput([]string{f.keyID})
	input["project_id"] = other
	h.want(owner, "POST", "/api/v1/code/pools", input, idem("cross-project"), 404)
	member := h.invite(owner, "code-member@example.test", "operator")
	memberID := h.want(member, "GET", "/api/v1/profile", nil, nil, 200)["id"].(string)
	f.assignOnly(memberID)
	member = login(h, "code-member@example.test")
	addMember(h, owner, f.project, memberID, "viewer")
	input = f.poolInput([]string{f.keyID})
	input["kind"], input["owner_user_id"] = "personal", memberID
	h.want(owner, "POST", "/api/v1/code/pools", input, idem("personal-wrong-issuer"), 422)
	input["api_key_ids"] = []string{}
	h.want(owner, "POST", "/api/v1/code/pools", input, idem("personal"), 201)
	for _, collection := range []string{"accounts", "pools", "routes", "budgets", "bindings", "attempts", "refusals", "token-windows"} {
		h.want(member, "GET", "/api/v1/code/"+collection+"?project_id="+other, nil, nil, 404)
	}
	f.noSyntheticInference(t)
}

func codePublicDecode[T any](t *testing.T, value any) T {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result T
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *codePublicFixture) assignOnly(userID string) {
	users := f.h.want(f.owner, "GET", "/api/v1/users", nil, nil, 200)["items"].([]any)
	for _, item := range users {
		user := item.(map[string]any)
		if user["id"] == userID {
			f.h.want(f.owner, "PATCH", "/api/v1/users/"+userID, map[string]any{"access_scope": "assigned"}, etagHeader(user), 200)
			return
		}
	}
	f.h.t.Fatal("fixture member missing from public user list")
}
