//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	guardrailsdk "github.com/tyk-swe/olp/sdk/guardrails"
)

func TestGuardrailPlatformActivationAttachmentsAndApply(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Guardrail platform")
	createCatalogRoute(h, owner, project, "guarded-platform")
	d := h.want(owner, "POST", "/api/v1/guardrails", map[string]any{"name": "Private words", "project_id": project, "type": "builtin.keywords", "configuration": map[string]any{"words": []string{"restricted"}}, "timeout_ms": 1000, "on_error": "fail_closed"}, idem(uuid.NewString()), 201)
	if d["active_revision_id"] != nil {
		t.Fatal("new typed definition did not start as a draft")
	}
	p := h.want(owner, "POST", "/api/v1/guardrail-policies", map[string]any{"name": "Project safeguards", "project_id": project, "entries": []any{map[string]any{"guardrail_id": d["id"], "revision_id": d["revision_id"], "phase": "input", "action": "block", "mandatory": true}}}, idem(uuid.NewString()), 201)
	attach := map[string]any{"scope": "project", "scope_id": project, "policy_revision_id": p["revision_id"], "order": 0}
	h.want(owner, "POST", "/api/v1/guardrail-attachments", attach, idem(uuid.NewString()), 422)
	h.want(owner, "POST", "/api/v1/guardrails/"+d["id"].(string)+"/activate", map[string]any{"revision_id": d["revision_id"]}, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + d["etag"].(string) + `"`}, 204)
	h.want(owner, "POST", "/api/v1/guardrail-policies/"+p["id"].(string)+"/activate", map[string]any{"revision_id": p["revision_id"]}, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + p["etag"].(string) + `"`}, 204)
	h.want(owner, "POST", "/api/v1/guardrail-attachments", attach, idem(uuid.NewString()), 201)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Guardrail caller", "project_id": project, "scopes": []string{"inference", "guardrails"}, "allowed_routes": []string{"guarded-platform"}}, idem(uuid.NewString()), 201)
	if err := h.Runtime.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	caller := sweepCaller{token: key["secret"].(string)}
	status, _, body := h.call(caller, "POST", "/v1/chat/completions", map[string]any{"model": "guarded-platform", "messages": []any{map[string]any{"role": "user", "content": "restricted"}}}, nil)
	if status != 400 || !strings.Contains(body, "guardrail_blocked") {
		t.Fatalf("attached guardrail: %d %s", status, body)
	}
	status, _, rawResult := h.call(caller, "POST", "/v1/guardrails/apply", map[string]any{"policy_revision_id": p["revision_id"], "phase": "input", "segments": []any{map[string]any{"id": "s", "text": "restricted"}}}, nil)
	if status != 200 {
		t.Fatalf("apply failed: %d %s", status, rawResult)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(rawResult), &result); err != nil {
		t.Fatal(err)
	}
	if result["blocked"] != true {
		t.Fatal("standalone policy did not block")
	}
	evidence, _ := json.Marshal(result["decisions"])
	if strings.Contains(string(evidence), "restricted") {
		t.Fatal("content in evidence")
	}
	console := h.want(owner, "POST", "/api/v1/guardrails/evaluate", map[string]any{"policy_revision_id": p["revision_id"], "phase": "input", "segments": []any{map[string]any{"id": "s", "text": "restricted"}}}, nil, 200)
	if console["blocked"] != true {
		t.Fatal("console evaluated a different policy")
	}
	client, err := guardrailsdk.NewClientWithResponses(h.HTTP.URL, guardrailsdk.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+key["secret"].(string))
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	sdkResult, err := client.ApplyGuardrailsWithResponse(t.Context(), guardrailsdk.GuardrailApplyInput{PolicyRevisionId: uuid.MustParse(p["revision_id"].(string)), Phase: guardrailsdk.GuardrailApplyInputPhaseInput, Segments: []guardrailsdk.GuardrailSegment{{Id: "sdk", Text: "restricted"}}})
	if err != nil || sdkResult.JSON200 == nil || !sdkResult.JSON200.Blocked {
		t.Fatalf("generated SDK evaluation failed: %v %+v", err, sdkResult)
	}
}

func TestGuardrailPlatformStrictRouteRejectsTransformingAttachment(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Strict safeguards")
	fixture := newOpenAIFixture(t, "")
	_, _, _, _ = provisionOpenAIInProject(t, h, owner, &project, fixture.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "strict"}}, map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})

	d := h.want(owner, "POST", "/api/v1/guardrails", map[string]any{"name": "PII", "project_id": project, "type": "builtin.pii", "configuration": map[string]any{}, "timeout_ms": 1000, "on_error": "fail_closed"}, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/guardrails/"+d["id"].(string)+"/activate", map[string]any{"revision_id": d["revision_id"]}, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + d["etag"].(string) + `"`}, 204)
	p := h.want(owner, "POST", "/api/v1/guardrail-policies", map[string]any{"name": "Mask PII", "project_id": project, "entries": []any{map[string]any{"guardrail_id": d["id"], "revision_id": d["revision_id"], "phase": "input", "action": "mask"}}}, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/guardrail-policies/"+p["id"].(string)+"/activate", map[string]any{"revision_id": p["revision_id"]}, map[string]string{"Idempotency-Key": uuid.NewString(), "If-Match": `"` + p["etag"].(string) + `"`}, 204)
	// The project contains a strict native route.
	h.want(owner, "POST", "/api/v1/guardrail-attachments", map[string]any{"scope": "project", "scope_id": project, "policy_revision_id": p["revision_id"], "order": 0}, idem(uuid.NewString()), 422)
}
