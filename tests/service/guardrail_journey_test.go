//go:build service

package service_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func testGuardrailJourney(t *testing.T, parent *service, owner *caller) {
	s := &service{t: t, pool: parent.pool, origin: parent.origin, env: parent.env}
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": uniqueName("guardrails")}, idempotent(), http.StatusCreated)["id"].(string)

	credential := s.want(owner, "POST", "/api/v1/guardrail-credentials", map[string]any{
		"project_id": project, "name": "vendor", "credential": map[string]any{"api_key": "journey-secret-value"},
	}, idempotent(), http.StatusCreated)
	credentialID := credential["id"].(string)
	status, raw := s.do(owner, "GET", "/api/v1/guardrail-credentials", nil, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), credentialID) || strings.Contains(string(raw), "journey-secret-value") {
		t.Fatalf("credential list: %s", describe(status, raw))
	}
	if got := s.want(owner, "GET", "/api/v1/guardrail-credentials/"+credentialID, nil, nil, http.StatusOK); got["project_id"] != project || got["name"] != "vendor" {
		t.Fatalf("credential = %v", got)
	}

	configuration := map[string]any{"endpoint": "https://guard.example.com/v2/guard", "credential_ref": credentialID, "content_egress": true}
	external := s.want(owner, "POST", "/api/v1/guardrails", map[string]any{
		"name": "vendor", "type": "lakera", "project_id": project, "configuration": configuration, "timeout_ms": 500, "on_error": "fail_open",
	}, idempotent(), http.StatusCreated)
	if sameJSON(t, external["configuration"]) != sameJSON(t, configuration) || external["on_error"] != "fail_open" {
		t.Fatalf("external guardrail did not round-trip its configuration: %v", external)
	}

	created := s.want(owner, "POST", "/api/v1/guardrails", map[string]any{
		"name": "keywords", "type": "builtin.keywords", "project_id": project, "configuration": map[string]any{"words": []string{"forbidden"}},
	}, idempotent(), http.StatusCreated)
	guardrail := "/api/v1/guardrails/" + created["id"].(string)
	change := map[string]any{"name": "keywords", "configuration": map[string]any{"words": []string{"forbidden", "banned"}}}
	once := idempotent(etag(created))
	updated := s.want(owner, "PUT", guardrail, change, once, http.StatusOK)
	if replayed := s.want(owner, "PUT", guardrail, change, once, http.StatusOK); replayed["revision_id"] != updated["revision_id"] || updated["revision"] != float64(2) {
		t.Fatalf("idempotent update = %v, first = %v", replayed, updated)
	}
	first := s.want(owner, "GET", guardrail+"/revisions/"+created["revision_id"].(string), nil, nil, http.StatusOK)
	if sameJSON(t, first["configuration"]) != `{"words":["forbidden"]}` {
		t.Fatalf("first revision = %v", first)
	}
	s.want(owner, "POST", guardrail+"/activate", map[string]any{"revision_id": updated["revision_id"]}, idempotent(etag(updated)), http.StatusNoContent)

	entry := map[string]any{"guardrail_id": created["id"], "revision_id": updated["revision_id"], "phase": "input", "action": "block"}
	policy := s.want(owner, "POST", "/api/v1/guardrail-policies", map[string]any{"name": "journey", "project_id": project, "entries": []any{entry}}, idempotent(), http.StatusCreated)
	policyPath := "/api/v1/guardrail-policies/" + policy["policy_id"].(string)
	s.want(owner, "POST", policyPath+"/activate", map[string]any{"revision_id": policy["revision_id"]}, idempotent(etag(policy)), http.StatusNoContent)
	attachment := s.want(owner, "POST", "/api/v1/guardrail-attachments", map[string]any{
		"scope": "project", "scope_id": project, "policy_revision_id": policy["revision_id"], "order": 0,
	}, idempotent(), http.StatusCreated)

	current := s.want(owner, "GET", policyPath, nil, nil, http.StatusOK)
	revised := s.want(owner, "PUT", policyPath, map[string]any{"name": "journey-revised", "entries": []any{entry}}, idempotent(etag(current)), http.StatusOK)
	if revised["revision"] != float64(2) || revised["active_revision_id"] != policy["revision_id"] {
		t.Fatalf("revised policy = %v", revised)
	}
	s.want(owner, "POST", policyPath+"/activate", map[string]any{"revision_id": revised["revision_id"]}, idempotent(etag(revised)), http.StatusNoContent)
	if original := s.want(owner, "GET", policyPath+"/revisions/"+policy["revision_id"].(string), nil, nil, http.StatusOK); original["revision_id"] != policy["revision_id"] || original["revision"] != float64(1) {
		t.Fatalf("first policy revision = %v", original)
	}
	attached := listedAttachment(t, s, owner, attachment["id"].(string))
	replaced := s.want(owner, "PUT", "/api/v1/guardrail-attachments/"+attachment["id"].(string), map[string]any{"policy_revision_id": revised["revision_id"]}, idempotent(etag(attached)), http.StatusOK)
	if replaced["policy_revision_id"] != revised["revision_id"] || replaced["etag"] == attached["etag"] {
		t.Fatalf("replaced attachment = %v", replaced)
	}

	evaluation := s.want(owner, "POST", "/api/v1/guardrails/evaluate", map[string]any{
		"policy_revision_id": revised["revision_id"], "phase": "input",
		"segments": []any{map[string]any{"id": "m1", "role": "user", "text": "this is banned content"}},
	}, nil, http.StatusOK)
	decisions := items(evaluation["decisions"])
	if evaluation["blocked"] != true || len(decisions) != 1 || decisions[0]["outcome"] != "blocked" || decisions[0]["guardrail_id"] != created["id"] {
		t.Fatalf("evaluation = %v", evaluation)
	}

	detach := idempotent(etag(replaced))
	s.want(owner, "DELETE", "/api/v1/guardrail-attachments/"+attachment["id"].(string), nil, detach, http.StatusNoContent)
	s.want(owner, "DELETE", "/api/v1/guardrail-attachments/"+attachment["id"].(string), nil, detach, http.StatusNoContent)
	if status, raw := s.do(owner, "GET", "/api/v1/guardrail-attachments", nil, nil); strings.Contains(string(raw), attachment["id"].(string)) {
		t.Fatalf("detached attachment is still listed: %s", describe(status, raw))
	}

	livePolicy := s.want(owner, "GET", policyPath, nil, nil, http.StatusOK)
	s.want(owner, "DELETE", policyPath, nil, idempotent(etag(livePolicy)), http.StatusNoContent)
	if retiredPolicy := s.want(owner, "GET", policyPath, nil, nil, http.StatusOK); retiredPolicy["retired_at"] == nil {
		t.Fatalf("retired policy = %v", retiredPolicy)
	}

	live := s.want(owner, "GET", guardrail, nil, nil, http.StatusOK)
	s.want(owner, "DELETE", guardrail, nil, idempotent(etag(live)), http.StatusNoContent)
	retired := s.want(owner, "GET", guardrail, nil, nil, http.StatusOK)
	if retired["retired_at"] == nil {
		t.Fatalf("retired guardrail = %v", retired)
	}
	if problem := s.want(owner, "PUT", guardrail, change, idempotent(etag(retired)), http.StatusConflict); !strings.HasSuffix(problem["type"].(string), "/guardrail_retired") {
		t.Fatalf("changing a retired guardrail = %v", problem)
	}

	if problem := s.want(owner, "DELETE", "/api/v1/guardrail-credentials/"+credentialID, nil, idempotent(map[string]string{"If-Match": `"` + credentialID + `"`}), http.StatusConflict); !strings.HasSuffix(problem["type"].(string), "/guardrail_credential_in_use") {
		t.Fatalf("deleting a referenced credential = %v", problem)
	}
	unused := s.want(owner, "POST", "/api/v1/guardrail-credentials", map[string]any{
		"project_id": project, "name": "unused", "credential": map[string]any{"api_key": "unused-secret-value"},
	}, idempotent(), http.StatusCreated)["id"].(string)
	s.want(owner, "DELETE", "/api/v1/guardrail-credentials/"+unused, nil, idempotent(map[string]string{"If-Match": `"` + unused + `"`}), http.StatusNoContent)
	s.want(owner, "GET", "/api/v1/guardrail-credentials/"+unused, nil, nil, http.StatusNotFound)

	resources := []string{credentialID, unused, external["id"].(string), created["id"].(string), policy["policy_id"].(string), attachment["id"].(string)}
	rows, err := s.pool.Query(t.Context(), `SELECT DISTINCT action COLLATE "C" FROM olp.audit WHERE resource_id::text=ANY($1) ORDER BY 1`, resources)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"guardrail.activate", "guardrail.attach", "guardrail.attachment.replace", "guardrail.content_egress_acknowledged",
		"guardrail.create", "guardrail.credential.create", "guardrail.credential.delete", "guardrail.detach",
		"guardrail.retire", "guardrail.update", "guardrail_policy.create", "guardrail_policy.retire", "guardrail_policy.update",
	}
	if !slices.Equal(actions, want) {
		t.Fatalf("audit actions = %v, want %v", actions, want)
	}
}

func listedAttachment(t *testing.T, s *service, owner *caller, id string) map[string]any {
	t.Helper()
	for _, attachment := range items(s.want(owner, "GET", "/api/v1/guardrail-attachments", nil, nil, http.StatusOK)["items"]) {
		if attachment["id"] == id {
			return attachment
		}
	}
	t.Fatalf("attachment %s is not listed", id)
	return nil
}

func sameJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
