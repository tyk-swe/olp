//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func routeFidelityMode(t *testing.T, value any) string {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatal("fidelity is not an explicit object", value)
	}
	mode, ok := object["mode"].(string)
	if !ok {
		t.Fatal("fidelity lacks its mode", value)
	}
	return mode
}
func fidelityPolicy(action, phase string) map[string]any {
	rule := map[string]any{"id": "content-rule", "phase": phase, "action": action, "pattern": "private-marker"}
	if action == "redact" {
		rule["replacement"] = "[MASK]"
	}
	return map[string]any{"rules": []any{rule}}
}
func fidelityDraft(slug string, provider any) map[string]any {
	return map[string]any{"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1, "targets": []any{map[string]any{"provider_id": provider, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}
}
func transformed(draft map[string]any) map[string]any {
	draft["fidelity"] = map[string]any{"mode": "transformed"}
	return draft
}
func strictDraft(draft map[string]any) map[string]any {
	draft["fidelity"] = map[string]any{"mode": "strict"}
	return draft
}
func requireFieldError(t *testing.T, problem map[string]any, field string) {
	t.Helper()
	errors, _ := problem["errors"].(map[string]any)
	if problemCode(t, problem) != "validation_failed" || errors[field] == nil {
		t.Fatalf("expected a typed %s field error: %v", field, problem)
	}
}

func TestRouteFidelityOmissionIsStrictAndLegacyIsRejected(t *testing.T) {
	h := newAccessHarness(t)
	fixture := newOpenAIFixture(t, "")
	owner, provider, slug, secret := provisionOpenAIWith(t, h, fixture.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "transformed"}})
	route := h.want(owner, "GET", "/api/v1/routes", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	if routeFidelityMode(t, route["latest_revision"].(map[string]any)["fidelity"]) != "transformed" {
		t.Fatal("published revision did not keep its explicit fidelity", route)
	}
	encoded, err := json.Marshal(h.Runtime.Release().Snapshot.Routes[slug])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"fidelity":{"mode":"transformed"}`) {
		t.Fatal("runtime snapshot omitted the route fidelity", string(encoded))
	}

	// Omission means strict for a published transformed slug too; nothing is
	// inherited from the published revision or from the draft being replaced.
	created := h.want(owner, "POST", "/api/v1/route-drafts", fidelityDraft(slug, provider["id"]), idem(uuid.NewString()), 201)
	if routeFidelityMode(t, created["fidelity"]) != "strict" {
		t.Fatal("omitted fidelity did not create a strict draft", created)
	}
	path := "/api/v1/route-drafts/" + created["id"].(string)
	draft := h.want(owner, "GET", path, nil, nil, 200)
	draft = h.want(owner, "PUT", path, transformed(fidelityDraft(slug, provider["id"])), etagHeader(draft), 200)
	if routeFidelityMode(t, draft["fidelity"]) != "transformed" {
		t.Fatal("explicit transformed replacement was not kept", draft)
	}
	for _, omitted := range []any{nil, map[string]any{}} {
		body := fidelityDraft(slug, provider["id"])
		if omitted != nil {
			body["fidelity"] = omitted
		}
		draft = h.want(owner, "PUT", path, body, etagHeader(draft), 200)
		if routeFidelityMode(t, draft["fidelity"]) != "strict" {
			t.Fatal("replacement without a mode inherited the previous fidelity", omitted, draft)
		}
		draft = h.want(owner, "PUT", path, transformed(fidelityDraft(slug, provider["id"])), etagHeader(draft), 200)
	}
	nullBody, err := json.Marshal(fidelityDraft(slug, provider["id"]))
	if err != nil {
		t.Fatal(err)
	}
	nullBody = []byte(strings.TrimSuffix(string(nullBody), "}") + `,"fidelity":null}`)
	draft = h.want(owner, "PUT", path, json.RawMessage(nullBody), etagHeader(draft), 200)
	if routeFidelityMode(t, draft["fidelity"]) != "strict" {
		t.Fatal("null fidelity did not declare a strict route", draft)
	}

	for _, invalid := range []any{map[string]any{"mode": "legacy"}, map[string]any{"mode": nil}, map[string]any{"mode": ""}, map[string]any{"mode": "native"}, map[string]any{"mode": "strict", "fallback": "transformed"}, []any{}} {
		bad := fidelityDraft(slug, provider["id"])
		bad["fidelity"] = invalid
		requireFieldError(t, h.want(owner, "PUT", path, bad, etagHeader(draft), 422), "fidelity")
		requireFieldError(t, h.want(owner, "POST", "/api/v1/route-drafts", bad, idem(uuid.NewString()), 422), "fidelity")
	}
	duplicate := []byte(strings.TrimSuffix(string(nullBody), `,"fidelity":null}`) + `,"fidelity":{"mode":"strict"},"fidelity":{"mode":"transformed"}}`)
	if problemCode(t, h.want(owner, "POST", "/api/v1/route-drafts", json.RawMessage(duplicate), idem(uuid.NewString()), 400)) != "invalid_json" {
		t.Fatal("duplicate fidelity members were decoded last-wins")
	}

	// Redaction is permitted only on transformed routes.
	for _, phase := range []string{"input", "output"} {
		body := fidelityDraft(slug, provider["id"])
		body["content_policy"] = fidelityPolicy("redact", phase)
		strict := h.want(owner, "POST", "/api/v1/route-drafts", body, idem(uuid.NewString()), 201)
		strictPath := "/api/v1/route-drafts/" + strict["id"].(string)
		for _, action := range []string{"validate", "activate"} {
			problem := h.want(owner, "POST", strictPath+"/"+action, nil, withMatch(strict, idem(uuid.NewString())), 422)
			if problemCode(t, problem) != "fidelity_policy_conflict" || !strings.Contains(problem["detail"].(string), "transformed route") {
				t.Fatal("strict redaction conflict was hidden by another activation condition", problem)
			}
		}
	}

	// A strict route whose target has no provider profile cannot activate, and
	// the refusal names the declaration that would serve it.
	release := h.Runtime.Release()
	body := fidelityDraft(slug, provider["id"])
	body["content_policy"] = fidelityPolicy("block", "input")
	draft = h.want(owner, "PUT", path, body, etagHeader(draft), 200)
	for _, action := range []string{"validate", "activate"} {
		problem := h.want(owner, "POST", path+"/"+action, nil, withMatch(draft, idem(uuid.NewString())), 422)
		if problemCode(t, problem) != "target_capability" || !strings.Contains(problem["detail"].(string), "Declare the route transformed") {
			t.Fatal("strict activation did not guide the author to a transformed route", problem)
		}
	}
	preview := h.want(owner, "POST", path+"/simulate", map[string]any{"operation": "generation", "surface": "openai", "mode": "unary", "seed": "strict-preview"}, nil, 200)
	inspection := preview["targets"].([]any)[0].(map[string]any)["decision"].(map[string]any)["interaction"].(map[string]any)
	if inspection["status"] != "not_inspected" || inspection["fidelity"] != "strict" || inspection["class"] != nil || inspection["effective_request"] != nil {
		t.Fatal("tuple-only simulation claimed semantic admission", inspection)
	}
	h.refresh()
	if h.Runtime.Release().Digest != release.Digest || h.Runtime.Release().Sequence != release.Sequence {
		t.Fatal("failed strict activation published a runtime revision")
	}
	if status, reply, _ := h.gateway("POST", "/v1/chat/completions", secret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "private-marker"}}}); status != http.StatusOK {
		t.Fatal("transformed route stopped serving", status, reply)
	}

	transformedBody := transformed(fidelityDraft(slug, provider["id"]))
	transformedBody["content_policy"] = fidelityPolicy("redact", "input")
	draft = h.want(owner, "PUT", path, transformedBody, etagHeader(draft), 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	h.refresh()
	if h.Runtime.Release().Snapshot.Routes[slug].Fidelity.Mode != "transformed" {
		t.Fatal("published fidelity was lost")
	}
	if status, reply, _ := h.gateway("POST", "/v1/chat/completions", secret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "private-marker"}}}); status != 200 {
		t.Fatal(status, reply)
	}
	sent, _ := json.Marshal(fixture.lastReq.Load())
	if !strings.Contains(string(sent), "[MASK]") || strings.Contains(string(sent), "private-marker") {
		t.Fatal("transformed route did not apply its redaction")
	}
}

// The same slug becomes transformed through an ordinary new revision.

// The gateway has not polled the new release. It must refuse new strict
// work immediately after publication, before any provider request.

// A failed authoritative read also refuses the strict admission.

// A retained strict response is refused on the transformed route.

// Restoring the strict revision activates it with its own fidelity.

// Omission is strict, so the exported v2 redaction attachment cannot inherit
// the published transformed contract.

// The explicit strict mode equals the omitted one, so the plan is a no-op.

// An Automatic provider (no provider profile) certified for realtime.
