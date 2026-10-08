//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestCodeRouteCanonicalizesIDs(t *testing.T) {
	f := newCodeFixture(t)
	f.exec(t, `UPDATE olp.api_keys SET policy=jsonb_set(policy,'{allowed_routes}','["coding","canonical-project"]') WHERE id=$1`, f.key)
	input := map[string]any{"project_id": strings.ToUpper(f.project), "slug": "canonical-project", "pool_id": strings.ToUpper(f.pool), "models": []string{"native-model"}, "enabled": true}
	path := "/api/v1/code/routes"
	headers := idem("canonical-project")
	for _, method := range []string{"POST", "PUT"} {
		t.Run(method, func(t *testing.T) {
			status := 200
			if method == "POST" {
				status = 201
			}
			draft := f.h.want(f.owner, method, path, input, headers, status)
			if draft["project_id"] != f.project || draft["pool_id"] != f.pool {
				t.Fatalf("draft IDs = %v/%v, want %s/%s", draft["project_id"], draft["pool_id"], f.project, f.pool)
			}
			path = "/api/v1/code/routes/" + draft["id"].(string)
			published := f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("canonical-publish-"+method)), 200)
			f.route = codePublicDecode[codemode.Route](t, published)
			headers = etagHeader(published)
			revisions := f.h.want(f.owner, "GET", path+"/revisions", nil, nil, 200)["items"].([]any)
			for _, revision := range revisions {
				stored := codePublicDecode[codemode.Route](t, revision.(map[string]any)["route"])
				if stored.ProjectID != f.project || stored.PoolID != f.pool {
					t.Fatalf("published IDs = %s/%s, want %s/%s", stored.ProjectID, stored.PoolID, f.project, f.pool)
				}
			}
			var document []byte
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT snapshot FROM olp.runtime_releases ORDER BY sequence DESC LIMIT 1`).Scan(&document); err != nil {
				t.Fatal(err)
			}
			var snapshot runtime.Snapshot
			if err := json.Unmarshal(document, &snapshot); err != nil {
				t.Fatal(err)
			}
			f.route = snapshot.CodeRoutes[f.route.Slug]
			if f.route.ProjectID != f.project || f.route.PoolID != f.pool {
				t.Fatalf("runtime IDs = %s/%s, want %s/%s", f.route.ProjectID, f.route.PoolID, f.project, f.pool)
			}
			permit, err := f.store.Admit(t.Context(), f.input("canonical-"+method, "", nil))
			if err != nil {
				t.Fatal("canonical route rejected an authorized key:", err)
			}
			if err := f.store.Abort(t.Context(), permit.Attempt.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodeRouteDraftRetainsActivePublication(t *testing.T) {
	f := newCodeFixture(t)
	path := "/api/v1/code/routes/" + f.route.ID
	input := map[string]any{"project_id": f.project, "slug": f.route.Slug, "pool_id": f.pool, "models": []string{"unpublished-model"}, "enabled": false}
	draft := f.h.want(f.owner, "PUT", path, input, map[string]string{"If-Match": `"` + f.route.ETag + `"`}, 200)
	check := func(route codemode.Route) {
		t.Helper()
		if route.RevisionID != f.route.RevisionID || route.Revision != 1 || route.PublishedAt == nil || !route.PublishedAt.Equal(*f.route.PublishedAt) {
			t.Fatalf("draft edit lost active publication: %+v", route)
		}
		if route.Enabled || !slices.Equal(route.Models, []string{"unpublished-model"}) || route.ETag == f.route.ETag {
			t.Fatalf("draft edit did not retain independent fields: %+v", route)
		}
	}
	check(codePublicDecode[codemode.Route](t, draft))
	list := func() codemode.Route {
		t.Helper()
		items := f.h.want(f.owner, "GET", "/api/v1/code/routes?project_id="+f.project, nil, nil, 200)["items"]
		return codePublicDecode[[]codemode.Route](t, items)[0]
	}
	check(list())
	f.exec(t, `UPDATE olp.code_routes SET draft=draft-'revision_id'-'revision'-'published_at' WHERE id=$1`, f.route.ID)
	check(list())
	config := f.h.want(f.owner, "GET", path+"/client-config?gateway_url=https://gateway.example", nil, nil, 200)
	if !slices.Equal(codePublicDecode[[]string](t, config["native_models"]), f.route.Models) {
		t.Fatal("client configuration uses draft models")
	}
	if _, err := f.store.Admit(t.Context(), f.input("published-until-replaced", "", nil)); err != nil {
		t.Fatal("draft disabled the active route", err)
	}
	published := f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("second-publication")), 200)
	if route := codePublicDecode[codemode.Route](t, published); route.Revision != 2 || route.RevisionID == f.route.RevisionID || route.Enabled {
		t.Fatalf("publication failed to replace revision: %+v", route)
	}
	if list().Revision != 2 {
		t.Fatal("list did not advance publication")
	}
	f.h.want(f.owner, "GET", path+"/client-config?gateway_url=https://gateway.example", nil, nil, 409)
}

// A revision whose frozen connections name subscription families stops
// serving client configuration when its pool stops serving them; it must not
// fabricate Codex configuration for an unserved mix.
func TestCodeRouteClientConfigurationFollowsFrozenSubscriptions(t *testing.T) {
	f := newCodeFixture(t)
	path := "/api/v1/code/routes/" + f.route.ID + "/client-config?gateway_url=https://gateway.example"
	// Give the fixture provider an adapter profile and republish, so the
	// route's frozen connections name a subscription family.
	f.exec(t, `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{profile_id}','"zai-coding-plan"') WHERE id=$1`, f.provider)
	draft := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, map[string]any{"project_id": f.project, "slug": f.route.Slug, "pool_id": f.pool, "models": []string{"native-model"}, "enabled": true}, map[string]string{"If-Match": `"` + f.route.ETag + `"`}, 200)
	f.route = codePublicDecode[codemode.Route](t, f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(draft, idem("adapter-publish")), 200))
	if config := f.h.want(f.owner, "GET", path, nil, nil, 200); fmt.Sprint(config["adapters"]) != "[zai_coding]" {
		t.Fatalf("served route adapters: %v", config["adapters"])
	}
	f.exec(t, `DELETE FROM olp.code_pool_accounts WHERE pool_id=$1`, f.pool)
	if problem := f.h.want(f.owner, "GET", path, nil, nil, 409); problem["type"] != "https://openllmproxy.dev/problems/code_route_unserved" {
		t.Fatalf("unserved route: %v", problem)
	}
	// A revision whose connections name no adapter keeps the Codex fallback
	// it had before adapters existed, even with no serving accounts.
	f.exec(t, `INSERT INTO olp.code_pool_accounts(pool_id,account_id) VALUES($1,$2)`, f.pool, f.account)
	f.exec(t, `UPDATE olp.providers SET configuration=configuration::jsonb-'profile_id' WHERE id=$1`, f.provider)
	draft = f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, map[string]any{"project_id": f.project, "slug": f.route.Slug, "pool_id": f.pool, "models": []string{"native-model"}, "enabled": true}, map[string]string{"If-Match": `"` + f.route.ETag + `"`}, 200)
	f.route = codePublicDecode[codemode.Route](t, f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(draft, idem("legacy-publish")), 200))
	f.exec(t, `DELETE FROM olp.code_pool_accounts WHERE pool_id=$1`, f.pool)
	if config := f.h.want(f.owner, "GET", path, nil, nil, 200); config["client"] != "codex" {
		t.Fatalf("legacy route client: %v", config["client"])
	}
}

// Generated configuration offers only what requests can reach: an account
// dispatch would refuse — disabled, its provider disabled, its credential
// revoked, or its grant lapsed or expired — stops advertising its models and
// client, and the route reports unserved once no eligible account remains.
func TestCodeRouteClientConfigurationSkipsIneligibleAccounts(t *testing.T) {
	f := newCodeFixture(t)
	path := "/api/v1/code/routes/" + f.route.ID + "/client-config?gateway_url=https://gateway.example&client=opencode"
	// Mix two families on one route, so each mutation leaves one serving.
	provider2, credential2 := access.NewID(), access.NewID()
	f.exec(t, `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{profile_id}','"zai-coding-plan"') WHERE id=$1`, f.provider)
	f.exec(t, `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,'Second coding subscription','plugin','draft','{"kind":"plugin","auth_mode":"grant","endpoint":"https://fixture.invalid","profile_id":"opencode-go"}',$2,$3,$4,$5)`, provider2, access.NewID(), access.NewID(), f.user, f.project)
	f.exec(t, `INSERT INTO olp.provider_credentials(id,provider_id,version,plugin_digest,principal,grant_facts) VALUES($1,$2,1,'fixture-digest','fixture-principal-2','{}')`, credential2, provider2)
	f.exec(t, `INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, credential2)
	account2 := f.h.want(f.owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": provider2, "credential_id": credential2, "name": "Second subscription", "enabled": true, "models": []string{"other-model"}}, idem("account-2"), 201)["id"].(string)
	f.exec(t, `INSERT INTO olp.code_pool_accounts(pool_id,account_id) VALUES($1,$2)`, f.pool, account2)
	draft := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, map[string]any{"project_id": f.project, "slug": f.route.Slug, "pool_id": f.pool, "models": []string{"native-model", "other-model"}, "enabled": true}, map[string]string{"If-Match": `"` + f.route.ETag + `"`}, 200)
	f.route = codePublicDecode[codemode.Route](t, f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(draft, idem("mixed-publish")), 200))
	want := func(t *testing.T, models, adapters string) {
		t.Helper()
		config := f.h.want(f.owner, "GET", path, nil, nil, 200)
		if fmt.Sprint(config["native_models"]) != models || fmt.Sprint(config["adapters"]) != adapters {
			t.Fatalf("configuration = %v/%v, want %v/%v", config["native_models"], config["adapters"], models, adapters)
		}
	}
	want(t, "[native-model other-model]", "[opencode_go zai_coding]")
	for _, c := range []struct {
		name, id, fail, fix string
	}{
		{"disabled account", f.account, `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, `UPDATE olp.code_accounts SET enabled=true WHERE id=$1`},
		{"disabled provider", f.provider, `UPDATE olp.providers SET state='disabled' WHERE id=$1`, `UPDATE olp.providers SET state='draft' WHERE id=$1`},
		{"lapsed grant", f.credential, `UPDATE olp.provider_grants SET lapsed_at=now() WHERE credential_id=$1`, `UPDATE olp.provider_grants SET lapsed_at=NULL WHERE credential_id=$1`},
		{"expired grant", f.credential, `UPDATE olp.provider_grants SET expires_at=now()-interval '1 hour' WHERE credential_id=$1`, `UPDATE olp.provider_grants SET expires_at=NULL WHERE credential_id=$1`},
		// Revocation is terminal: no fix restores the account.
		{"revoked credential", f.credential, `UPDATE olp.provider_credentials SET revoked_at=now() WHERE id=$1`, ``},
	} {
		t.Run(c.name, func(t *testing.T) {
			f.exec(t, c.fail, c.id)
			want(t, "[other-model]", "[opencode_go]")
			if c.fix != "" {
				f.exec(t, c.fix, c.id)
				want(t, "[native-model other-model]", "[opencode_go zai_coding]")
			}
		})
	}
	f.exec(t, `UPDATE olp.code_accounts SET enabled=false WHERE id IN ($1,$2)`, f.account, account2)
	if problem := f.h.want(f.owner, "GET", path, nil, nil, 409); problem["type"] != "https://openllmproxy.dev/problems/code_route_unserved" {
		t.Fatalf("unserved route: %v", problem)
	}
}

func TestCodeRouteSlugsReservedAcrossDraftsAndPublication(t *testing.T) {
	f := newCodeFixture(t)
	f.exec(t, `INSERT INTO olp.provider_models(id,provider_id,upstream_model,display_name) VALUES($1,$2,$3,'Fixture model')`, access.NewID(), f.provider, vendorModel)
	ordinary := func(slug string) map[string]any {
		in := fidelityDraft(slug, f.provider)
		in["project_id"] = f.project
		return in
	}
	code := func(slug string) map[string]any {
		return map[string]any{"project_id": f.project, "slug": slug, "pool_id": f.pool, "models": []string{"native-model"}, "enabled": true}
	}
	f.h.want(f.owner, "POST", "/api/v1/code/routes", code("unpublished"), idem("unpublished-code"), 201)
	free := f.h.want(f.owner, "POST", "/api/v1/route-drafts", ordinary("ordinary"), idem("ordinary"), 201)
	path := "/api/v1/route-drafts/" + free["id"].(string)
	for _, slug := range []string{"coding", "unpublished"} {
		requireFieldError(t, f.h.want(f.owner, "POST", "/api/v1/route-drafts", ordinary(slug), idem("collision-"+slug), 422), "slug")
		requireFieldError(t, f.h.want(f.owner, "PUT", path, ordinary(slug), etagHeader(free), 422), "slug")
	}
	requireFieldError(t, f.h.want(f.owner, "POST", "/api/v1/code/routes", code("ordinary"), idem("reverse-collision"), 422), "slug")
	f.exec(t, `UPDATE olp.route_drafts SET slug='coding' WHERE id=$1`, free["id"])
	requireFieldError(t, f.h.want(f.owner, "POST", path+"/validate", nil, etagHeader(free), 422), "slug")
	requireFieldError(t, f.h.want(f.owner, "POST", path+"/activate", nil, withMatch(free, idem("legacy-collision")), 422), "slug")
}
