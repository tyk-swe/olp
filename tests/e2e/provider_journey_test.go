//go:build e2e

package e2e_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestProviderLifecycleJourney(t *testing.T) {
	t.Parallel()
	parent := startService(t)
	owner := parent.installationToken(parent.owner(), "providers")
	project := parent.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "providers"}, idempotent(), http.StatusCreated)["id"].(string)
	upstream := newVendor(t).URL
	configuration := func(extra map[string]any) map[string]any {
		document := map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": upstream + "/v1"}
		for field, value := range extra {
			document[field] = value
		}
		return document
	}
	journey := func(t *testing.T) (*service, string) {
		t.Parallel()
		s := &service{t: t, pool: parent.pool, origin: parent.origin, env: parent.env}
		provider := s.activeProvider(owner, uniqueName("provider"), upstream, project)
		return s, "/api/v1/providers/" + provider["id"].(string)
	}

	t.Run("revisions_restore_and_reactivate", func(t *testing.T) {
		s, path := journey(t)
		original := s.want(owner, "GET", path, nil, nil, http.StatusOK)
		renamed := uniqueName("renamed")
		draft := s.want(owner, "PATCH", path, map[string]any{"name": renamed, "configuration": original["configuration"]}, etag(original), http.StatusOK)
		if draft["pending_activation"] != true {
			t.Fatalf("renamed draft = %v, want a pending activation", draft)
		}
		s.want(owner, "POST", path+"/activate", nil, idempotent(etag(draft)), http.StatusOK)

		revisions := items(s.want(owner, "GET", path+"/revisions", nil, nil, http.StatusOK)["items"])
		if len(revisions) != 2 || revisions[0]["revision"] != 2.0 || revisions[0]["name"] != renamed || revisions[1]["revision"] != 1.0 || revisions[1]["name"] != original["name"] {
			t.Fatalf("revisions = %v, want 2 (%s) then 1 (%s)", revisions, renamed, original["name"])
		}
		diff := s.want(owner, "GET", path+"/revisions/diff?from=1&to=2", nil, nil, http.StatusOK)
		if diff["from_revision"] != 1.0 || diff["to_revision"] != 2.0 || diff["name_changed"] != true || diff["endpoint_changed"] != false || diff["connector_changed"] != false || !empty(diff["models_added"]) || !empty(diff["models_removed"]) || !empty(diff["capabilities_removed"]) {
			t.Fatalf("diff = %v, want only the name to change", diff)
		}

		current := s.want(owner, "GET", path, nil, nil, http.StatusOK)
		restored := s.want(owner, "POST", path+"/revisions/"+text(revisions[1], "id")+"/restore-as-draft", nil, idempotent(etag(current)), http.StatusOK)
		draft = restored["provider"].(map[string]any)
		if restored["credential_restored"] != false || draft["name"] != original["name"] || draft["pending_activation"] != true {
			t.Fatalf("restore = %v, want revision 1 pending activation without its credential", restored)
		}
		s.want(owner, "POST", path+"/activate", nil, idempotent(etag(draft)), http.StatusOK)

		active := s.want(owner, "GET", path, nil, nil, http.StatusOK)
		if active["state"] != "active" || active["active_revision"] != 3.0 || active["name"] != original["name"] || active["pending_activation"] != false {
			t.Fatalf("provider = %v, want revision 3 active under the original name", active)
		}
		latest := s.want(owner, "GET", path+"/revisions/3", nil, nil, http.StatusOK)
		if latest["name"] != original["name"] || latest["certified_capability_count"] != 2.0 {
			t.Fatalf("revision 3 = %v, want the restored name with its certified evidence", latest)
		}
	})

	t.Run("slots_validate_and_delete_with_preconditions", func(t *testing.T) {
		s, path := journey(t)
		slots := s.want(owner, "GET", path+"/credential-slots", nil, nil, http.StatusOK)
		second := uuid.NewString()
		slots = s.want(owner, "PUT", path+"/credential-slots/"+second, map[string]any{"slot": map[string]any{"name": "second"}, "credential": vendor.Secret}, idempotent(etag(slots)), http.StatusOK)
		if validatedAt(t, slots, second) != nil {
			t.Fatalf("new slot health = %v, want it unvalidated", slots["health"])
		}
		slots = s.want(owner, "POST", path+"/credential-slots/"+second+"/validate", nil, nil, http.StatusOK)
		if validatedAt(t, slots, second) == nil {
			t.Fatalf("validated slot health = %v, want a validation time", slots["health"])
		}

		detail := s.want(owner, "GET", path, nil, nil, http.StatusOK)
		s.want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": configuration(map[string]any{"options": map[string]any{"parameter_defaults": map[string]any{"temperature": 0.2}}})}, etag(detail), http.StatusOK)
		slots = s.want(owner, "GET", path+"/credential-slots", nil, nil, http.StatusOK)
		for _, slot := range items(slots["items"]) {
			if validatedAt(t, slots, text(slot, "id")) != nil {
				t.Fatalf("slot health after a transport change = %v, want every slot unvalidated", slots["health"])
			}
		}

		stale := s.want(owner, "GET", path+"/credential-slots/"+second, nil, nil, http.StatusOK)
		s.want(owner, "PUT", path+"/credential-slots/"+second, map[string]any{"slot": map[string]any{"name": "second renamed"}}, idempotent(etag(stale)), http.StatusOK)
		current := s.want(owner, "GET", path+"/credential-slots/"+second, nil, nil, http.StatusOK)
		if current["name"] != "second renamed" || current["etag"] == stale["etag"] {
			t.Fatalf("renamed slot = %v, want a new name and etag", current)
		}
		if problem := s.want(owner, "DELETE", path+"/credential-slots/"+second, nil, idempotent(etag(stale)), http.StatusPreconditionFailed); !strings.HasSuffix(problem["type"].(string), "/etag_mismatch") {
			t.Fatalf("deleting a slot with a stale etag = %v, want etag_mismatch", problem)
		}
		s.want(owner, "DELETE", path+"/credential-slots/"+second, nil, idempotent(etag(current)), http.StatusNoContent)
		s.want(owner, "GET", path+"/credential-slots/"+second, nil, nil, http.StatusNotFound)
		if remaining := items(s.want(owner, "GET", path+"/credential-slots", nil, nil, http.StatusOK)["items"]); len(remaining) != 1 || remaining[0]["id"] == second {
			t.Fatalf("slots after delete = %v, want only the default slot", remaining)
		}
	})

	t.Run("network_credentials_reference_and_revoke", func(t *testing.T) {
		s, path := journey(t)
		detail := s.want(owner, "GET", path, nil, nil, http.StatusOK)
		created := s.want(owner, "POST", path+"/network-credentials", map[string]any{"credential": `{"proxy_username":"proxy","proxy_password":"secret"}`}, idempotent(etag(detail)), http.StatusCreated)
		id := text(created, "credential_id")
		listed := items(s.want(owner, "GET", path+"/network-credentials", nil, nil, http.StatusOK)["items"])
		if len(listed) != 1 || listed[0]["id"] != id || listed[0]["version"] != 1.0 || listed[0]["revoked_at"] != nil {
			t.Fatalf("network credentials = %v, want version 1 of %s unrevoked", listed, id)
		}

		referenced := configuration(map[string]any{"options": map[string]any{"network": map[string]any{"credential_id": id}}})
		detail = s.want(owner, "GET", path, nil, nil, http.StatusOK)
		detail = s.want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": referenced}, etag(detail), http.StatusOK)
		network := detail["configuration"].(map[string]any)["options"].(map[string]any)["network"].(map[string]any)
		if network["credential_id"] != id {
			t.Fatalf("configuration network = %v, want a reference to %s", network, id)
		}

		revoked := s.want(owner, "POST", path+"/network-credentials/"+id+"/revoke", nil, idempotent(etag(detail)), http.StatusOK)
		if revoked["credential_id"] != id {
			t.Fatalf("revoke = %v, want %s", revoked, id)
		}
		listed = items(s.want(owner, "GET", path+"/network-credentials", nil, nil, http.StatusOK)["items"])
		if len(listed) != 1 || listed[0]["revoked_at"] == nil {
			t.Fatalf("network credentials = %v, want %s revoked", listed, id)
		}
		detail = s.want(owner, "GET", path, nil, nil, http.StatusOK)
		problem := s.want(owner, "PATCH", path, map[string]any{"name": uniqueName("renamed"), "configuration": referenced}, etag(detail), http.StatusUnprocessableEntity)
		if _, ok := problem["errors"].(map[string]any)["configuration.options.network.credential_id"]; !ok {
			t.Fatalf("draft referencing a revoked network credential blamed %v, want the network credential reference", problem["errors"])
		}
	})

	t.Run("disable_retains_history_and_delete_replays", func(t *testing.T) {
		s, path := journey(t)
		detail := s.want(owner, "GET", path, nil, nil, http.StatusOK)
		s.want(owner, "POST", path+"/disable", nil, idempotent(etag(detail)), http.StatusOK)
		disabled := s.want(owner, "GET", path, nil, nil, http.StatusOK)
		if disabled["state"] != "disabled" {
			t.Fatalf("provider = %v, want disabled", disabled)
		}
		if problem := s.want(owner, "DELETE", path, nil, idempotent(etag(disabled)), http.StatusConflict); !strings.HasSuffix(problem["type"].(string), "/provider_in_use") {
			t.Fatalf("deleting a disabled provider = %v, want provider_in_use", problem)
		}
		if revisions := items(s.want(owner, "GET", path+"/revisions", nil, nil, http.StatusOK)["items"]); len(revisions) != 1 {
			t.Fatalf("revisions of a disabled provider = %v, want its published revision retained", revisions)
		}

		draft := s.want(owner, "POST", "/api/v1/providers", map[string]any{
			"name": uniqueName("draft"), "configuration": configuration(nil), "credential": vendor.Secret, "model": vendor.Model, "project_id": project,
		}, idempotent(), http.StatusCreated)
		draftPath := "/api/v1/providers/" + text(draft, "id")
		deletion := idempotent(etag(draft))
		for range 2 {
			if status, raw := s.do(owner, "DELETE", draftPath, nil, deletion); status != http.StatusNoContent || len(raw) != 0 {
				t.Fatalf("delete with one idempotency key: %s, want 204 every time", describe(status, raw))
			}
		}
		s.want(owner, "GET", draftPath, nil, nil, http.StatusNotFound)
	})
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
