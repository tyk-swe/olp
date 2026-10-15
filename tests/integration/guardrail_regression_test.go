//go:build integration

package integration_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestGuardrailPublicationValidatesCredentialBoundaries(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Guardrail credential owner")
	other := createProject(h, owner, "Other credential owner")
	credential := func(project, name string) string {
		return h.want(owner, "POST", "/api/v1/guardrail-credentials", map[string]any{"name": name, "project_id": project, "credential": map[string]any{"api_key": "fixture-only"}}, idem(uuid.NewString()), 201)["id"].(string)
	}
	owned, foreign := credential(project, "owned"), credential(other, "foreign")
	for _, network := range []bool{false, true} {
		for _, ref := range []string{uuid.NewString(), foreign, owned} {
			t.Run(fmt.Sprintf("network=%v/ref=%s", network, ref), func(t *testing.T) {
				config := map[string]any{"endpoint": "https://example.com", "credential_ref": ref, "content_egress": true}
				if network {
					config["credential_ref"] = owned
					config["network"] = map[string]any{"credential_id": ref}
				}
				d := h.want(owner, "POST", "/api/v1/guardrails", map[string]any{"name": uuid.NewString(), "project_id": project, "type": "webhook", "configuration": config, "timeout_ms": 1000, "on_error": "fail_closed"}, idem(uuid.NewString()), 201)
				var releases int
				if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.runtime_releases").Scan(&releases); err != nil {
					t.Fatal(err)
				}
				status := 422
				if ref == owned {
					status = 204
				}
				h.want(owner, "POST", "/api/v1/guardrails/"+d["id"].(string)+"/activate", map[string]any{"revision_id": d["revision_id"]}, withMatch(d, idem(uuid.NewString())), status)
				if ref == owned {
					if err := h.Runtime.Refresh(t.Context()); err != nil {
						t.Fatalf("published release could not be installed: %v", err)
					}
					return
				}
				var inactive, unrecorded bool
				var after int
				if err := h.Pool.QueryRow(t.Context(), `SELECT active_revision_id IS NULL,NOT EXISTS(SELECT 1 FROM olp.guardrail_revision_activations WHERE revision_id=$2) FROM olp.guardrails WHERE id=$1`, d["id"], d["revision_id"]).Scan(&inactive, &unrecorded); err != nil {
					t.Fatal(err)
				}
				if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.runtime_releases").Scan(&after); err != nil {
					t.Fatal(err)
				}
				if !inactive || !unrecorded || after != releases {
					t.Fatalf("failed publication was not rolled back: inactive=%v unrecorded=%v releases=%d/%d", inactive, unrecorded, releases, after)
				}
			})
		}
	}
}

func TestGuardrailConfigurationActivatesMatchingDefinitionDraft(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	projectName := "Matching draft"
	project := createProject(h, owner, projectName)
	input := map[string]any{"name": "private", "project_id": project, "type": "builtin.keywords", "configuration": map[string]any{"words": []string{"old"}}, "timeout_ms": 1000, "on_error": "fail_closed"}
	d := h.want(owner, "POST", "/api/v1/guardrails", input, idem(uuid.NewString()), 201)
	path := "/api/v1/guardrails/" + d["id"].(string)
	h.want(owner, "POST", path+"/activate", map[string]any{"revision_id": d["revision_id"]}, withMatch(d, idem(uuid.NewString())), 204)
	d = h.want(owner, "GET", path, nil, nil, 200)
	input["configuration"] = map[string]any{"words": []string{"new"}}
	delete(input, "project_id")
	delete(input, "type")
	draft := h.want(owner, "PUT", path, input, withMatch(d, idem(uuid.NewString())), 200)
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	doc["guardrails"].([]any)[0].(map[string]any)["configuration"] = input["configuration"]
	doc["guardrail_policies"] = []any{map[string]any{"name": "policy", "project": projectName, "entries": []any{map[string]any{"guardrail": "private", "phase": "input", "action": "block"}}}}
	for range 2 {
		h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem(uuid.NewString()), 200)
	}
	current := h.want(owner, "GET", path, nil, nil, 200)
	if current["active_revision_id"] != draft["revision_id"] || current["revision_id"] != draft["revision_id"] {
		t.Fatalf("matching draft was not activated: %v", current)
	}
	var pinned string
	var revisions int
	if err := h.Pool.QueryRow(t.Context(), `SELECT v.entries->0->>'revision_id' FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.id=p.active_revision_id WHERE p.project_id=$1 AND p.name='policy'`, project).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.guardrail_revisions WHERE guardrail_id=$1", d["id"]).Scan(&revisions); err != nil {
		t.Fatal(err)
	}
	if pinned != draft["revision_id"] || revisions != 2 {
		t.Fatalf("promotion pinned the wrong revision or appended a duplicate: %s %d", pinned, revisions)
	}
}

func TestGuardrailConfigurationResolvesPolicyDrafts(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, matching := range []bool{false, true} {
			t.Run(fmt.Sprintf("active=%v/matching=%v", active, matching), func(t *testing.T) {
				h := newAccessHarness(t)
				owner := h.owner()
				projectName := "Policy draft"
				project := createProject(h, owner, projectName)
				d := h.want(owner, "POST", "/api/v1/guardrails", map[string]any{"name": "private", "project_id": project, "type": "builtin.keywords", "configuration": map[string]any{"words": []string{"secret"}}, "timeout_ms": 1000, "on_error": "fail_closed"}, idem(uuid.NewString()), 201)
				h.want(owner, "POST", "/api/v1/guardrails/"+d["id"].(string)+"/activate", map[string]any{"revision_id": d["revision_id"]}, withMatch(d, idem(uuid.NewString())), 204)
				rule := func(action string) []any {
					return []any{map[string]any{"guardrail_id": d["id"], "revision_id": d["revision_id"], "phase": "input", "action": action}}
				}
				input := map[string]any{"name": "policy", "project_id": project, "entries": rule("block")}
				p := h.want(owner, "POST", "/api/v1/guardrail-policies", input, idem(uuid.NewString()), 201)
				path := "/api/v1/guardrail-policies/" + p["id"].(string)
				version, action := 1, "block"
				if active {
					h.want(owner, "POST", path+"/activate", map[string]any{"revision_id": p["revision_id"]}, withMatch(p, idem(uuid.NewString())), 204)
					p = h.want(owner, "GET", path, nil, nil, 200)
					action, version = "annotate", 2
					input["entries"] = rule(action)
					p = h.want(owner, "PUT", path, input, withMatch(p, idem(uuid.NewString())), 200)
				}
				if !matching {
					action = "redact"
					version++
				}
				doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
				doc["guardrail_policies"] = []any{map[string]any{"name": "policy", "project": projectName, "entries": []any{map[string]any{"guardrail": "private", "phase": "input", "action": action}}}}
				for range 2 {
					h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem(uuid.NewString()), 200)
				}
				var got, count int
				var activated bool
				var gotAction string
				if err := h.Pool.QueryRow(t.Context(), `SELECT v.revision,p.active_revision_id=p.latest_revision_id,v.entries->0->>'action',(SELECT count(*) FROM olp.guardrail_policy_revisions WHERE policy_id=p.id) FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.id=p.active_revision_id WHERE p.id=$1`, p["id"]).Scan(&got, &activated, &gotAction, &count); err != nil {
					t.Fatal(err)
				}
				if got != version || count != version || !activated || gotAction != action {
					t.Fatalf("wrong policy promotion: revision=%d count=%d activated=%v action=%s", got, count, activated, gotAction)
				}
			})
		}
	}
}

func TestGuardrailConfigurationUpdatesInstallationDefinition(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	entry := map[string]any{"name": "global", "project": "", "type": "builtin.keywords", "configuration": map[string]any{"words": []string{"old"}}, "timeout_ms": 1000, "on_error": "fail_closed"}
	doc["guardrails"] = []any{entry}
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem(uuid.NewString()), 200)
	entry["configuration"] = map[string]any{"words": []string{"new"}}
	for range 2 {
		h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem(uuid.NewString()), 200)
	}
	var version int
	var word string
	if err := h.Pool.QueryRow(t.Context(), `SELECT v.revision,v.configuration->'words'->>0 FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.id=g.active_revision_id WHERE g.project_id IS NULL AND g.name='global'`).Scan(&version, &word); err != nil {
		t.Fatal(err)
	}
	if version != 2 || word != "new" {
		t.Fatalf("installation definition not updated: %d %s", version, word)
	}
}
