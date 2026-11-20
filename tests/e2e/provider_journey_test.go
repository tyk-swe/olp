//go:build e2e

package e2e_test

import (
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestProviderLifecycleJourney(t *testing.T) {
	t.Parallel()
	parent := startService(t)
	owner := parent.installationToken(parent.Owner(), "providers")
	project := parent.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "providers"}, harness.Idempotent(), http.StatusCreated)["id"].(string)
	upstream := newVendor(t).URL
	configuration := func(extra map[string]any) map[string]any {
		document := map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": upstream + "/v1"}
		maps.Copy(document, extra)
		return document
	}
	journey := func(t *testing.T) (*service, string) {
		t.Parallel()
		s := &service{&harness.Service{T: t, Pool: parent.Pool, Origin: parent.Origin, Env: parent.Env}}
		provider := s.activeProvider(owner, uniqueName("provider"), upstream, project)
		return s, "/api/v1/providers/" + provider["id"].(string)
	}

	t.Run("template_preserves_route_retirement", func(t *testing.T) {
		t.Parallel()
		s := &service{&harness.Service{T: t, Pool: parent.Pool, Origin: parent.Origin, Env: parent.Env}}
		project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": uniqueName("retired-template")}, harness.Idempotent(), http.StatusCreated)["id"].(string)
		prefix := uniqueName("retired-template")
		s.Want(owner, "POST", "/api/v1/route-templates", map[string]any{
			"name": prefix, "provider_selector": "vendor:" + vendors.DefaultFor("openai_compatible"), "model_filter": vendor.Model,
			"slug_pattern": prefix + "-{model}", "overall_timeout_ms": 5000, "max_attempts": 1,
			"fidelity": map[string]any{"mode": "transformed"}, "auto_publish": true, "project_id": project,
		}, harness.Idempotent(), http.StatusCreated)
		s.activeProvider(owner, uniqueName("first-template-provider"), upstream, project)
		routes := items(s.Want(owner, "GET", "/api/v1/routes", nil, nil, http.StatusOK)["items"])
		index := slices.IndexFunc(routes, func(route map[string]any) bool { return route["slug"] == prefix+"-"+vendor.Model })
		if index < 0 {
			t.Fatal("template did not publish a route")
		}
		path := "/api/v1/routes/" + text(routes[index], "id")
		s.activeProvider(owner, uniqueName("second-template-provider"), upstream, project)
		active := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		revisions := items(s.Want(owner, "GET", path+"/revisions", nil, nil, http.StatusOK)["items"])
		if active["state"] != "active" || len(revisions) != 2 {
			t.Fatalf("unretired automatic join failed: %v %v", active, revisions)
		}
		s.Want(owner, "POST", path+"/retire", nil, harness.Idempotent(harness.ETag(active)), http.StatusOK)
		s.activeProvider(owner, uniqueName("third-template-provider"), upstream, project)
		retired := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		if retired["state"] != "retired" {
			t.Errorf("provider activation reactivated retired route: %v", retired["state"])
		}
		after := items(s.Want(owner, "GET", path+"/revisions", nil, nil, http.StatusOK)["items"])
		if len(after) != len(revisions) {
			t.Errorf("retired route gained an automatic revision: %d -> %d", len(revisions), len(after))
		}
		restored := s.Want(owner, "POST", path+"/revisions/"+text(after[0], "id")+"/restore-as-draft", nil, harness.Idempotent(), http.StatusCreated)
		s.Want(owner, "POST", "/api/v1/route-drafts/"+text(restored, "id")+"/activate", nil, harness.Idempotent(harness.ETag(restored)), http.StatusOK)
		if got := s.Want(owner, "GET", path, nil, nil, http.StatusOK); got["state"] != "active" {
			t.Fatalf("explicit restoration failed: %v", got["state"])
		}
	})

	t.Run("revisions_restore_and_reactivate", func(t *testing.T) {
		s, path := journey(t)
		original := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		renamed := uniqueName("renamed")
		draft := s.Want(owner, "PATCH", path, map[string]any{"name": renamed, "configuration": original["configuration"]}, harness.ETag(original), http.StatusOK)
		if draft["pending_activation"] != true {
			t.Fatalf("renamed draft = %v, want a pending activation", draft)
		}
		s.Want(owner, "POST", path+"/activate", nil, harness.Idempotent(harness.ETag(draft)), http.StatusOK)

		revisions := items(s.Want(owner, "GET", path+"/revisions", nil, nil, http.StatusOK)["items"])
		if len(revisions) != 2 || revisions[0]["revision"] != 2.0 || revisions[0]["name"] != renamed || revisions[1]["revision"] != 1.0 || revisions[1]["name"] != original["name"] {
			t.Fatalf("revisions = %v, want 2 (%s) then 1 (%s)", revisions, renamed, original["name"])
		}
		diff := s.Want(owner, "GET", path+"/revisions/diff?from=1&to=2", nil, nil, http.StatusOK)
		if diff["from_revision"] != 1.0 || diff["to_revision"] != 2.0 || diff["name_changed"] != true || diff["endpoint_changed"] != false || diff["connector_changed"] != false || !empty(diff["models_added"]) || !empty(diff["models_removed"]) || !empty(diff["capabilities_removed"]) {
			t.Fatalf("diff = %v, want only the name to change", diff)
		}

		current := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		restored := s.Want(owner, "POST", path+"/revisions/"+text(revisions[1], "id")+"/restore-as-draft", nil, harness.Idempotent(harness.ETag(current)), http.StatusOK)
		draft = restored["provider"].(map[string]any)
		if restored["credential_restored"] != false || draft["name"] != original["name"] || draft["pending_activation"] != true {
			t.Fatalf("restore = %v, want revision 1 pending activation without its credential", restored)
		}
		s.Want(owner, "POST", path+"/activate", nil, harness.Idempotent(harness.ETag(draft)), http.StatusOK)

		active := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		if active["state"] != "active" || active["active_revision"] != 3.0 || active["name"] != original["name"] || active["pending_activation"] != false {
			t.Fatalf("provider = %v, want revision 3 active under the original name", active)
		}
		latest := s.Want(owner, "GET", path+"/revisions/3", nil, nil, http.StatusOK)
		if latest["name"] != original["name"] || latest["certified_capability_count"] != 2.0 {
			t.Fatalf("revision 3 = %v, want the restored name with its certified evidence", latest)
		}
	})

	t.Run("slots_validate_and_delete_with_preconditions", func(t *testing.T) {
		s, path := journey(t)
		slots := s.Want(owner, "GET", path+"/credential-slots", nil, nil, http.StatusOK)
		second := uuid.NewString()
		slots = s.Want(owner, "PUT", path+"/credential-slots/"+second, map[string]any{"slot": map[string]any{"name": "second"}, "credential": vendor.Secret}, harness.Idempotent(harness.ETag(slots)), http.StatusOK)
		if validatedAt(t, slots, second) != nil {
			t.Fatalf("new slot health = %v, want it unvalidated", slots["health"])
		}
		slots = s.Want(owner, "POST", path+"/credential-slots/"+second+"/validate", nil, nil, http.StatusOK)
		if validatedAt(t, slots, second) == nil {
			t.Fatalf("validated slot health = %v, want a validation time", slots["health"])
		}

		detail := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		s.Want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": configuration(map[string]any{"options": map[string]any{"parameter_defaults": map[string]any{"temperature": 0.2}}})}, harness.ETag(detail), http.StatusOK)
		slots = s.Want(owner, "GET", path+"/credential-slots", nil, nil, http.StatusOK)
		for _, slot := range items(slots["items"]) {
			if validatedAt(t, slots, text(slot, "id")) != nil {
				t.Fatalf("slot health after a transport change = %v, want every slot unvalidated", slots["health"])
			}
		}

		stale := s.Want(owner, "GET", path+"/credential-slots/"+second, nil, nil, http.StatusOK)
		s.Want(owner, "PUT", path+"/credential-slots/"+second, map[string]any{"slot": map[string]any{"name": "second renamed"}}, harness.Idempotent(harness.ETag(stale)), http.StatusOK)
		current := s.Want(owner, "GET", path+"/credential-slots/"+second, nil, nil, http.StatusOK)
		if current["name"] != "second renamed" || current["etag"] == stale["etag"] {
			t.Fatalf("renamed slot = %v, want a new name and etag", current)
		}
		if problem := s.Want(owner, "DELETE", path+"/credential-slots/"+second, nil, harness.Idempotent(harness.ETag(stale)), http.StatusPreconditionFailed); !strings.HasSuffix(problem["type"].(string), "/etag_mismatch") {
			t.Fatalf("deleting a slot with a stale etag = %v, want etag_mismatch", problem)
		}
		s.Want(owner, "DELETE", path+"/credential-slots/"+second, nil, harness.Idempotent(harness.ETag(current)), http.StatusNoContent)
		s.Want(owner, "GET", path+"/credential-slots/"+second, nil, nil, http.StatusNotFound)
		if remaining := items(s.Want(owner, "GET", path+"/credential-slots", nil, nil, http.StatusOK)["items"]); len(remaining) != 1 || remaining[0]["id"] == second {
			t.Fatalf("slots after delete = %v, want only the default slot", remaining)
		}
	})

	t.Run("discovered_facts_reset_certified_evidence", func(t *testing.T) {
		t.Parallel()
		s := &service{&harness.Service{T: t, Pool: parent.Pool, Origin: parent.Origin, Env: parent.Env}}
		reportsFacts := atomic.Bool{}
		fixture := vendor.Handler()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/v1/models" && reportsFacts.Load() {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+vendor.Model+`","object":"model","inputTokenLimit":128000}]}`)
				return
			}
			fixture.ServeHTTP(w, r)
		}))
		t.Cleanup(server.Close)
		path := "/api/v1/providers/" + s.activeProvider(owner, uniqueName("provider"), server.URL, project)["id"].(string)

		reportsFacts.Store(true)
		detail := s.Want(owner, "POST", path+"/discovery", map[string]any{}, harness.ETag(s.Want(owner, "GET", path, nil, nil, http.StatusOK)), http.StatusOK)
		if detail["pending_activation"] != true {
			t.Fatalf("provider after discovering new facts = %v, want a pending activation", detail)
		}
		for _, model := range items(s.Want(owner, "GET", path+"/models", nil, nil, http.StatusOK)["items"]) {
			for _, capability := range items(model["capabilities"]) {
				if capability["source"] != "declared" {
					t.Fatalf("capability after discovering new facts = %v, want it declared again", capability)
				}
			}
		}
	})

	t.Run("network_credentials_reference_and_revoke", func(t *testing.T) {
		s, path := journey(t)
		detail := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		created := s.Want(owner, "POST", path+"/network-credentials", map[string]any{"credential": `{"proxy_username":"proxy","proxy_password":"secret"}`}, harness.Idempotent(harness.ETag(detail)), http.StatusCreated)
		id := text(created, "credential_id")
		reference := map[string]any{"store": "vault", "secret_id": "proxy", "version": "1"}
		detail = s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		s.Want(owner, "POST", path+"/network-credentials", map[string]any{"credential": `{"proxy_username":"other","proxy_password":"secret"}`, "credential_reference": reference}, harness.Idempotent(harness.ETag(detail)), http.StatusBadRequest)
		listed := items(s.Want(owner, "GET", path+"/network-credentials", nil, nil, http.StatusOK)["items"])
		if len(listed) != 1 || listed[0]["id"] != id || listed[0]["version"] != 1.0 || listed[0]["revoked_at"] != nil {
			t.Fatalf("network credentials = %v, want version 1 of %s unrevoked", listed, id)
		}

		referenced := configuration(map[string]any{"options": map[string]any{"network": map[string]any{"credential_id": id}}})
		detail = s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		detail = s.Want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": referenced}, harness.ETag(detail), http.StatusOK)
		network := detail["configuration"].(map[string]any)["options"].(map[string]any)["network"].(map[string]any)
		if network["credential_id"] != id {
			t.Fatalf("configuration network = %v, want a reference to %s", network, id)
		}

		revoked := s.Want(owner, "POST", path+"/network-credentials/"+id+"/revoke", nil, harness.Idempotent(harness.ETag(detail)), http.StatusOK)
		if revoked["credential_id"] != id {
			t.Fatalf("revoke = %v, want %s", revoked, id)
		}
		listed = items(s.Want(owner, "GET", path+"/network-credentials", nil, nil, http.StatusOK)["items"])
		if len(listed) != 1 || listed[0]["revoked_at"] == nil {
			t.Fatalf("network credentials = %v, want %s revoked", listed, id)
		}
		detail = s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		problem := s.Want(owner, "PATCH", path, map[string]any{"name": uniqueName("renamed"), "configuration": referenced}, harness.ETag(detail), http.StatusUnprocessableEntity)
		if _, ok := problem["errors"].(map[string]any)["configuration.options.network.credential_id"]; !ok {
			t.Fatalf("draft referencing a revoked network credential blamed %v, want the network credential reference", problem["errors"])
		}
	})

	t.Run("disable_retains_history_and_delete_replays", func(t *testing.T) {
		s, path := journey(t)
		detail := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		s.Want(owner, "POST", path+"/disable", nil, harness.Idempotent(harness.ETag(detail)), http.StatusOK)
		disabled := s.Want(owner, "GET", path, nil, nil, http.StatusOK)
		if disabled["state"] != "disabled" {
			t.Fatalf("provider = %v, want disabled", disabled)
		}
		if problem := s.Want(owner, "DELETE", path, nil, harness.Idempotent(harness.ETag(disabled)), http.StatusConflict); !strings.HasSuffix(problem["type"].(string), "/provider_in_use") {
			t.Fatalf("deleting a disabled provider = %v, want provider_in_use", problem)
		}
		if revisions := items(s.Want(owner, "GET", path+"/revisions", nil, nil, http.StatusOK)["items"]); len(revisions) != 1 {
			t.Fatalf("revisions of a disabled provider = %v, want its published revision retained", revisions)
		}

		draft := s.Want(owner, "POST", "/api/v1/providers", map[string]any{
			"name": uniqueName("draft"), "configuration": configuration(nil), "credential": vendor.Secret, "model": vendor.Model, "project_id": project,
		}, harness.Idempotent(), http.StatusCreated)
		draftPath := "/api/v1/providers/" + text(draft, "id")
		secrets := s.grantSecrets(text(draft, "id"))
		deletion := harness.Idempotent(harness.ETag(draft))
		for range 2 {
			if status, raw := s.Do(owner, "DELETE", draftPath, nil, deletion); status != http.StatusNoContent || len(raw) != 0 {
				t.Fatalf("delete with one idempotency key: %s, want 204 every time", describe(status, raw))
			}
		}
		s.Want(owner, "GET", draftPath, nil, nil, http.StatusNotFound)
		var left int
		if err := s.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE id=ANY($1::uuid[])", secrets).Scan(&left); err != nil || left != 0 {
			t.Fatalf("deleting a draft provider left %d of its credential, refresh and enrollment secrets (%v)", left, err)
		}
	})
}

func (s *service) grantSecrets(provider string) []string {
	s.T.Helper()
	refresh, enrollment := uuid.NewString(), uuid.NewString()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.secrets(id,purpose,key_version,ciphertext,expires_at) VALUES($1,'provider_grant_refresh',1,'\x00',now()+interval '1 hour'),($2,'grant_enrollment',1,'\x00',now()+interval '1 hour')`, []any{refresh, enrollment}},
		{`INSERT INTO olp.provider_grants(credential_id,refresh_token_id,expires_at,refresh_at) SELECT id,$2,now()+interval '1 hour',now()+interval '1 hour' FROM olp.provider_credentials WHERE provider_id=$1`, []any{provider, refresh}},
		{`INSERT INTO olp.grant_enrollments(id,provider_id,slot_id,plugin_digest,profile_id,started_by,expires_at)
			SELECT $2,p.id,s.id,repeat('0',64),'fixture-profile',p.created_by,now()+interval '1 hour' FROM olp.providers p JOIN olp.provider_slots s ON s.provider_id=p.id AND s.is_default WHERE p.id=$1`, []any{provider, enrollment}},
	} {
		if _, err := s.Pool.Exec(s.T.Context(), statement.sql, statement.args...); err != nil {
			s.T.Fatal(err)
		}
	}
	rows, err := s.Pool.Query(s.T.Context(), "SELECT id::text FROM olp.provider_credentials WHERE provider_id=$1", provider)
	if err != nil {
		s.T.Fatal(err)
	}
	credentials, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(credentials) == 0 {
		s.T.Fatalf("draft provider credentials = %v (%v), want its pasted credential", credentials, err)
	}
	return append(credentials, refresh, enrollment)
}

func validatedAt(t *testing.T, slots map[string]any, id string) any {
	t.Helper()
	health, ok := slots["health"].(map[string]any)[id].(map[string]any)
	if !ok {
		t.Fatalf("slot health = %v, want an entry for %s", slots["health"], id)
	}
	return health["validated_at"]
}

func empty(list any) bool {
	entries, ok := list.([]any)
	return ok && len(entries) == 0
}

func TestAReplayedCredentialRotationIsRefusedOnceTheCallerNoLongerManagesItsProject(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.Owner()
	project := s.Want(owner, "POST", "/api/v1/projects", map[string]any{"name": "handed-over"}, harness.Idempotent(), http.StatusCreated)
	projectID := project["id"].(string)
	const email = "former-manager@example.com"
	invited := s.Invite(owner, email, authz.RoleOperator)
	profile := s.Want(invited, "GET", "/api/v1/profile", nil, nil, http.StatusOK)
	userID := profile["id"].(string)
	s.Want(owner, "PATCH", "/api/v1/users/"+userID, map[string]any{"access_scope": "assigned"}, harness.ETag(profile), http.StatusOK)
	membership := "/api/v1/projects/" + projectID + "/members/" + userID
	s.Want(owner, "PUT", membership, map[string]any{"role": "manager"}, harness.ETag(project), http.StatusOK)
	manager := s.Login(email)

	draft := s.Want(manager, "POST", "/api/v1/providers", map[string]any{
		"name": uniqueName("rotated"), "credential": vendor.Secret, "model": vendor.Model, "project_id": projectID,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": newVendor(t).URL + "/v1"},
	}, harness.Idempotent(), http.StatusCreated)
	path := "/api/v1/providers/" + text(draft, "id") + "/credentials"
	rotate := harness.Idempotent(harness.ETag(draft))
	rotated := s.Want(manager, "POST", path, map[string]any{"credential": vendor.Secret}, rotate, http.StatusCreated)
	if replayed := s.Want(manager, "POST", path, map[string]any{"credential": vendor.Secret}, rotate, http.StatusCreated); replayed["credential_id"] != rotated["credential_id"] {
		t.Fatalf("replayed rotation stored credential %v, want the first rotation's %v", replayed["credential_id"], rotated["credential_id"])
	}
	if actions := auditActions(t, s, text(rotated, "credential_id")); !slices.Equal(actions, []string{"provider.credential.rotate"}) {
		t.Fatalf("audit for one rotation sent twice = %v, want one rotation", actions)
	}

	managed := s.Want(owner, "GET", "/api/v1/projects/"+projectID, nil, nil, http.StatusOK)
	s.Want(owner, "PUT", membership, map[string]any{"role": "viewer"}, harness.ETag(managed), http.StatusOK)
	if status, raw := s.Do(s.Login(email), "POST", path, map[string]any{"credential": vendor.Secret}, rotate); status != http.StatusForbidden {
		t.Fatalf("replayed rotation after losing the project: %s", describe(status, raw))
	}
}
