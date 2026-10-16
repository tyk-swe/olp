//go:build integration

package integration_test

import (
	"testing"

	"github.com/google/uuid"
)

func attachRetainedGuardrail(t *testing.T, h *accessHarness, owner *browser, slug, phase string) {
	t.Helper()
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	doc["guardrails"] = []any{map[string]any{"name": "retained guard", "project": "", "type": "builtin.keywords", "configuration": map[string]any{"words": []string{"restricted"}}, "timeout_ms": 1000, "on_error": "fail_closed"}}
	doc["guardrail_policies"] = []any{map[string]any{"name": "retained safeguards", "project": "", "entries": []any{map[string]any{"guardrail": "retained guard", "phase": phase, "action": "block", "mandatory": true}}}}
	doc["guardrail_attachments"] = []any{map[string]any{"scope": "installation", "policy": "retained safeguards", "policy_project": "", "order": 0}}
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": doc}, idem(uuid.NewString()), 200)
	policies := h.want(owner, "GET", "/api/v1/guardrail-policies", nil, nil, 200)["items"].([]any)
	policy := policies[0].(map[string]any)
	h.want(owner, "POST", "/api/v1/guardrail-policies/"+policy["id"].(string)+"/activate", map[string]any{"revision_id": policy["revision_id"]}, withMatch(policy, idem(uuid.NewString())), 204)
	h.refresh()
	if h.Runtime.Release().Snapshot.GuardrailEngine(slug, "") == nil {
		t.Fatal("retained fixture policy was not published")
	}
}
