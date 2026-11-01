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

func TestToolsetGuardrailAttachmentsSurviveConfigurationPromotion(t *testing.T) {
	source := newAccessHarness(t)
	owner := source.owner()
	for _, projectName := range []string{"Tool safeguards", "Other safeguards"} {
		prefix := strings.ReplaceAll(projectName, " ", "-")
		project := createProject(source, owner, projectName)
		toolset := source.want(owner, "POST", "/api/v1/toolsets", map[string]any{
			"project_id": project, "name": "Guarded tools", "toolset": map[string]any{"tools": []any{}},
		}, idem(prefix+"-toolset"), 201)
		definition := source.want(owner, "POST", "/api/v1/guardrails", map[string]any{
			"name": "Private words", "project_id": project, "type": "builtin.keywords",
			"configuration": map[string]any{"words": []string{"restricted"}}, "timeout_ms": 1000, "on_error": "fail_closed",
		}, idem(prefix+"-definition"), 201)
		source.want(owner, "POST", "/api/v1/guardrails/"+definition["id"].(string)+"/activate", map[string]any{"revision_id": definition["revision_id"]}, withMatch(definition, idem(prefix+"-activate")), 204)
		policy := source.want(owner, "POST", "/api/v1/guardrail-policies", map[string]any{
			"name": "Tool policy", "project_id": project, "entries": []any{map[string]any{
				"guardrail_id": definition["id"], "revision_id": definition["revision_id"], "phase": "input", "action": "block",
			}},
		}, idem(prefix+"-policy"), 201)
		source.want(owner, "POST", "/api/v1/guardrail-policies/"+policy["id"].(string)+"/activate", map[string]any{"revision_id": policy["revision_id"]}, withMatch(policy, idem(prefix+"-policy-activate")), 204)
		source.want(owner, "POST", "/api/v1/guardrail-attachments", map[string]any{
			"scope": "toolset", "scope_id": toolset["id"], "policy_revision_id": policy["revision_id"], "order": 0,
		}, idem(prefix+"-attach"), 201)
	}
	exported := source.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := mcpExecObject(t, exported, "document")
	attachments := mcpExecItems(t, document, "guardrail_attachments")
	if len(attachments) != 2 || mcpExecItem(t, attachments, 0, document)["name"] != "Guarded tools" || mcpExecItem(t, attachments, 1, document)["name"] != "Guarded tools" {
		t.Fatalf("configuration lost the portable toolset attachment: %v", attachments)
	}
	destination := newAccessHarness(t)
	destinationOwner := destination.owner()
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("tool-guardrail-promote"), 200)
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("tool-guardrail-promote-again"), 200)
	promoted := destination.want(destinationOwner, "GET", "/api/v1/toolsets", nil, nil, 200)
	promotedTools := mcpExecItems(t, promoted, "items")
	if len(promotedTools) != 2 {
		t.Fatalf("promotion did not create the toolset: %v", promotedTools)
	}
	listed := destination.want(destinationOwner, "GET", "/api/v1/guardrail-attachments", nil, nil, 200)
	installed := mcpExecItems(t, listed, "items")
	if len(installed) != 2 {
		t.Fatalf("promotion lost the guardrail attachment: %v", installed)
	}
	for i := range installed {
		attachment := mcpExecItem(t, installed, i, listed)
		if attachment["scope"] != "toolset" || (attachment["scope_id"] != mcpExecItem(t, promotedTools, 0, promoted)["id"] && attachment["scope_id"] != mcpExecItem(t, promotedTools, 1, promoted)["id"]) {
			t.Fatalf("promotion bound the policy to the wrong resource: %v", attachment)
		}
	}
}

// The project contains a strict native route.
