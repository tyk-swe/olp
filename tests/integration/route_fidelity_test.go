//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func routeFidelityMode(t *testing.T, value any) string {
	t.Helper()
	if value == nil {
		return "legacy"
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatal("fidelity is not an object", value)
	}
	mode, ok := object["mode"].(string)
	if !ok {
		t.Fatal("fidelity lacks its effective mode", value)
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

func TestRouteFidelityDraftsRemainExplicitAndStrictActivationFailsClosed(t *testing.T) {
	h := newAccessHarness(t)
	fixture := newOpenAIFixture(t, "")
	owner, provider, slug, secret := provisionOpenAI(t, h, fixture.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
	listed := h.want(owner, "GET", "/api/v3/routes", nil, nil, 200)["items"].([]any)
	route := listed[0].(map[string]any)
	routePath := "/api/v3/routes/" + route["id"].(string)
	if route["latest_revision"].(map[string]any)["fidelity"] != nil {
		t.Fatal("historical route acquired an explicit contract")
	}
	release := h.Runtime.Release()
	originalDigest, sequence := release.Digest, release.Sequence
	encoded, err := json.Marshal(release.Snapshot.Routes[slug])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"fidelity"`) {
		t.Fatal("legacy fidelity changed published snapshot bytes")
	}

	for _, phase := range []string{"input", "output"} {
		body := fidelityDraft(slug, provider["id"])
		body["fidelity"] = map[string]any{}
		body["content_policy"] = fidelityPolicy("redact", phase)
		draft := h.want(owner, "POST", "/api/v3/route-drafts", body, idem(uuid.NewString()), 201)
		if routeFidelityMode(t, draft["fidelity"]) != "strict" {
			t.Fatal("explicit empty contract did not default to strict")
		}
		path := "/api/v3/route-drafts/" + draft["id"].(string)
		for _, action := range []string{"validate", "activate"} {
			problem := h.want(owner, "POST", path+"/"+action, nil, withMatch(draft, idem(uuid.NewString())), 422)
			if problemCode(t, problem) != "fidelity_policy_conflict" {
				t.Fatal("strict redaction conflict was hidden by another activation condition", problem)
			}
		}
	}
	body := fidelityDraft(slug, provider["id"])
	body["fidelity"] = map[string]any{}
	body["content_policy"] = fidelityPolicy("block", "input")
	draft := h.want(owner, "POST", "/api/v3/route-drafts", body, idem(uuid.NewString()), 201)
	path := "/api/v3/route-drafts/" + draft["id"].(string)
	beforeValidation := draft
	if problemCode(t, h.want(owner, "POST", path+"/validate", nil, etagHeader(draft), 422)) != "target_capability" {
		t.Fatal("strict draft accepted an implicit legacy provider profile")
	}
	draft = h.want(owner, "PUT", path, body, etagHeader(draft), 200)
	oldClient := fidelityDraft(slug, provider["id"])
	oldClient["content_policy"] = fidelityPolicy("block", "input")
	oldClient["max_attempts"] = 2
	h.want(owner, "PUT", path, oldClient, etagHeader(beforeValidation), 412)
	draft = h.want(owner, "PUT", path, oldClient, etagHeader(draft), 200)
	if routeFidelityMode(t, draft["fidelity"]) != "strict" {
		t.Fatal("old-client edit downgraded the draft")
	}
	problem := h.want(owner, "POST", path+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 422)
	if problemCode(t, problem) != "target_capability" {
		t.Fatal("strict route reached the legacy dispatcher", problem)
	}
	preview := h.want(owner, "POST", path+"/simulate", map[string]any{"operation": "generation", "surface": "openai", "mode": "unary", "seed": "strict-preview"}, nil, 200)
	inspection := preview["targets"].([]any)[0].(map[string]any)["decision"].(map[string]any)["interaction"].(map[string]any)
	if inspection["status"] != "not_inspected" || inspection["fidelity"] != "strict" || inspection["class"] != nil || inspection["effective_request"] != nil {
		t.Fatal("tuple-only simulation claimed semantic admission", inspection)
	}
	h.refresh()
	if h.Runtime.Release().Digest != originalDigest || h.Runtime.Release().Sequence != sequence {
		t.Fatal("failed strict activation published a runtime revision")
	}
	if status, reply, _ := h.gateway("POST", "/v1/chat/completions", secret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}); status != http.StatusOK {
		t.Fatal("legacy route stopped serving", status, reply)
	}

	for _, invalid := range []any{nil, map[string]any{"mode": nil}, map[string]any{"mode": ""}, map[string]any{"mode": "native"}, map[string]any{"mode": "strict", "fallback": "legacy"}} {
		bad := fidelityDraft(slug, provider["id"])
		bad["fidelity"] = invalid
		h.want(owner, "PUT", path, bad, etagHeader(draft), 422)
	}
	duplicate, err := json.Marshal(fidelityDraft(slug, provider["id"]))
	if err != nil {
		t.Fatal(err)
	}
	duplicate = []byte(strings.TrimSuffix(string(duplicate), "}") + `,"fidelity":{"mode":"strict"},"fidelity":{"mode":"legacy"}}`)
	if problemCode(t, h.want(owner, "POST", "/api/v3/route-drafts", json.RawMessage(duplicate), idem(uuid.NewString()), 400)) != "invalid_json" {
		t.Fatal("duplicate fidelity members were decoded last-wins")
	}

	transformed := fidelityDraft(slug, provider["id"])
	transformed["fidelity"] = map[string]any{"mode": "transformed"}
	transformed["content_policy"] = fidelityPolicy("redact", "input")
	draft = h.want(owner, "PUT", path, transformed, etagHeader(draft), 200)
	activated := h.want(owner, "POST", path+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	h.refresh()
	if h.Runtime.Release().Snapshot.Routes[slug].Fidelity.Mode != "transformed" {
		t.Fatal("published contract was lost")
	}
	if status, reply, _ := h.gateway("POST", "/v1/chat/completions", secret, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "private-marker"}}}); status != 200 {
		t.Fatal(status, reply)
	}
	sent, _ := json.Marshal(fixture.lastReq.Load())
	if !strings.Contains(string(sent), "[MASK]") || strings.Contains(string(sent), "private-marker") {
		t.Fatal("explicit transformed route did not preserve its policy behavior")
	}
	diff := h.want(owner, "GET", routePath+"/revisions/diff?from=1&to=2", nil, nil, 200)
	if diff["fidelity_changed"] != true || diff["fidelity_before"] != nil || routeFidelityMode(t, diff["fidelity_after"]) != "transformed" {
		t.Fatal("revision diff omitted contract transition", diff)
	}
	restored := h.want(owner, "POST", routePath+"/revisions/"+activated["revision_id"].(string)+"/restore-as-draft", nil, idem(uuid.NewString()), 201)
	if routeFidelityMode(t, restored["fidelity"]) != "transformed" {
		t.Fatal("restore changed the revision contract")
	}
	created := h.want(owner, "POST", "/api/v3/route-drafts", fidelityDraft(slug, provider["id"]), idem(uuid.NewString()), 201)
	if routeFidelityMode(t, created["fidelity"]) != "transformed" {
		t.Fatal("same-slug old-client draft silently downgraded the published contract")
	}
	rollback := fidelityDraft(slug, provider["id"])
	rollback["fidelity"] = map[string]any{"mode": "legacy"}
	restorePath := "/api/v3/route-drafts/" + restored["id"].(string)
	restored = h.want(owner, "PUT", restorePath, rollback, etagHeader(restored), 200)
	h.want(owner, "POST", restorePath+"/activate", nil, withMatch(restored, idem(uuid.NewString())), 200)
	diff = h.want(owner, "GET", routePath+"/revisions/diff?from=2&to=3", nil, nil, 200)
	if diff["fidelity_changed"] != true || routeFidelityMode(t, diff["fidelity_after"]) != "legacy" {
		t.Fatal("explicit legacy transition was not reviewable", diff)
	}
}

func TestRouteFidelityConfigurationPromotionPreservesOmittedContracts(t *testing.T) {
	h := newAccessHarness(t)
	fixture := newOpenAIFixture(t, "")
	owner, provider, slug, _ := provisionOpenAIWith(t, h, fixture.URL, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"}, map[string]any{"fidelity": map[string]any{"mode": "transformed"}, "content_policy": fidelityPolicy("redact", "input")})
	export := h.want(owner, "GET", "/api/v3/configuration/export", nil, nil, 200)
	document := export["document"].(map[string]any)
	route := document["routes"].([]any)[0].(map[string]any)
	if routeFidelityMode(t, route["fidelity"]) != "transformed" {
		t.Fatal("export omitted explicit fidelity")
	}
	if digest := h.want(owner, "GET", "/api/v3/configuration/export", nil, nil, 200)["digest"]; digest != export["digest"] {
		t.Fatal("unchanged export digest drifted")
	}
	delete(route, "fidelity")
	h.want(owner, "POST", "/api/v3/configuration/apply", map[string]any{"document": document}, idem(uuid.NewString()), 200)
	var inherited bool
	for _, item := range h.want(owner, "GET", "/api/v3/route-drafts", nil, nil, 200)["items"].([]any) {
		d := item.(map[string]any)
		if d["slug"] == slug && d["state"] == "draft" && d["based_on_revision_id"] == nil {
			inherited = routeFidelityMode(t, d["fidelity"]) == "transformed"
		}
	}
	if !inherited {
		t.Fatal("old artifact creating a draft downgraded the published contract")
	}
	route["fidelity"] = map[string]any{}
	route["content_policy"] = fidelityPolicy("block", "input")
	h.want(owner, "POST", "/api/v3/configuration/apply", map[string]any{"document": document}, idem(uuid.NewString()), 200)
	var draft map[string]any
	for _, item := range h.want(owner, "GET", "/api/v3/route-drafts", nil, nil, 200)["items"].([]any) {
		d := item.(map[string]any)
		if d["slug"] == slug && d["state"] == "draft" && d["based_on_revision_id"] == nil {
			draft = d
			break
		}
	}
	if draft == nil || routeFidelityMode(t, draft["fidelity"]) != "strict" {
		t.Fatal("import did not stage strict contract", draft)
	}
	path := "/api/v3/route-drafts/" + draft["id"].(string)
	if problemCode(t, h.want(owner, "POST", path+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 422)) != "target_capability" {
		t.Fatal("imported strict draft activated")
	}
	// Explicitly migrate the provider profile so this draft can be validated by
	// the real strict compiler while leaving the published route unchanged.
	providerPath := "/api/v3/providers/" + provider["id"].(string)
	currentProvider := h.want(owner, "GET", providerPath, nil, nil, 200)
	config := currentProvider["configuration"].(map[string]any)
	config["profile_id"], config["profile_revision"] = "azure-legacy-chat", "1"
	h.want(owner, "PATCH", providerPath, map[string]any{"name": currentProvider["name"], "configuration": config}, etagHeader(currentProvider), 200)
	certifyProfileNetworkProvider(t, h, owner, provider["id"].(string))
	// Validating an unpublished draft must not make promotion forget its mode
	// and stage another draft under the published route's older contract.
	h.want(owner, "POST", path+"/validate", nil, etagHeader(draft), 200)
	delete(route, "fidelity")
	route["max_attempts"] = 2
	h.want(owner, "POST", "/api/v3/configuration/apply", map[string]any{"document": document}, idem(uuid.NewString()), 200)
	draft = h.want(owner, "GET", path, nil, nil, 200)
	if routeFidelityMode(t, draft["fidelity"]) != "strict" || draft["max_attempts"] != float64(2) {
		t.Fatal("old artifact edit downgraded staged contract", draft)
	}

	route["content_policy"] = fidelityPolicy("redact", "input")
	planned := h.want(owner, "POST", "/api/v3/configuration/plan", map[string]any{"document": document}, nil, 200)
	if !strings.Contains(fmt.Sprint(planned["conflicts"]), "fidelity_policy_conflict") {
		t.Fatal("import plan ignored inherited strict policy conflict", planned)
	}
	h.want(owner, "POST", "/api/v3/configuration/apply", map[string]any{"document": document}, idem(uuid.NewString()), 409)
	unchanged := h.want(owner, "GET", path, nil, nil, 200)
	if unchanged["etag"] != draft["etag"] {
		t.Fatal("conflicting import mutated the draft")
	}
	route["fidelity"] = map[string]any{"mode": "strict"}
	if problemCode(t, h.want(owner, "POST", "/api/v3/configuration/plan", map[string]any{"document": document}, nil, 422)) != "fidelity_policy_conflict" {
		t.Fatal("explicit imported strict redaction was accepted")
	}
	route["fidelity"] = map[string]any{"mode": "legacy"}
	h.want(owner, "POST", "/api/v3/configuration/apply", map[string]any{"document": document}, idem(uuid.NewString()), 200)
	draft = h.want(owner, "GET", path, nil, nil, 200)
	if routeFidelityMode(t, draft["fidelity"]) != "legacy" {
		t.Fatal("explicit import transition was ignored")
	}
	h.refresh()
	if h.Runtime.Release().Snapshot.Routes[slug].Fidelity.Mode != "transformed" {
		t.Fatal("staging a contract silently changed serving behavior")
	}
}
