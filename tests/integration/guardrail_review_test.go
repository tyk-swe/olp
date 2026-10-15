//go:build integration

package integration_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestGuardrailRetainedResponseUsesPinnedReleasePolicies(t *testing.T) {
	for _, historical := range []bool{false, true} {
		name := "current"
		if historical {
			name = "historical"
		}
		t.Run(name, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			fixture := newStrictProviderFixture(t, "azure-v1-responses")
			slug, _ := publishStrictProvider(t, h, owner, fixture, nil, nil, "strict")
			key := stateKey(t, h, owner, slug, true)
			h.refresh()
			status, raw, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(`{"model":"`+slug+`","input":"hello","store":true}`), map[string]string{"Content-Type": "application/json"})
			var first struct{ ID string }
			if status != 200 || json.Unmarshal(raw, &first) != nil || first.ID == "" {
				t.Fatalf("create retained response: %d %s", status, raw)
			}
			if historical {
				draft := fidelityDraft(slug, fixture.providerID)
				draft["fidelity"] = map[string]any{"mode": "strict"}
				created := h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
				h.want(owner, "POST", "/api/v1/route-drafts/"+created["id"].(string)+"/activate", nil, withMatch(created, idem(uuid.NewString())), 200)
			}
			attachRetainedGuardrail(t, h, owner, slug, "input")
			h.refresh()
			fixture.mu.Lock()
			before := len(fixture.calls)
			fixture.mu.Unlock()
			status, raw, _ = h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(`{"model":"`+slug+`","input":"restricted","store":false,"previous_response_id":"`+first.ID+`"}`), map[string]string{"Content-Type": "application/json"})
			fixture.mu.Lock()
			after := len(fixture.calls)
			fixture.mu.Unlock()
			if status != 400 || !strings.Contains(string(raw), "guardrail_blocked") || before != after {
				t.Fatalf("retained policy bypass: %d %s calls=%d/%d", status, raw, before, after)
			}
		})
	}
}

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

func TestGuardrailRetainedOutputRefusesBeforeProvider(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	fixture := newStrictProviderFixture(t, "azure-v1-responses")
	slug, _ := publishStrictProvider(t, h, owner, fixture, nil, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	attachRetainedGuardrail(t, h, owner, slug, "output")
	h.refresh()
	fixture.mu.Lock()
	before := len(fixture.calls)
	fixture.mu.Unlock()
	for _, fields := range []string{`"store":true`, `"store":true,"background":true`} {
		status, raw, _ := h.gatewayRaw("POST", "/v1/responses", key, strings.NewReader(`{"model":"`+slug+`","input":"hello",`+fields+`}`), map[string]string{"Content-Type": "application/json"})
		if status != 422 || !strings.Contains(string(raw), "guardrail_surface_unavailable") {
			t.Fatalf("unguarded retained output was admitted: %d %s", status, raw)
		}
	}
	fixture.mu.Lock()
	after := len(fixture.calls)
	fixture.mu.Unlock()
	if before != after {
		t.Fatalf("unsupported retained output reached the provider: %d/%d", before, after)
	}
}

func TestGuardrailConfigurationPromotesCredentialBindings(t *testing.T) {
	source, destination := newAccessHarness(t), newAccessHarness(t)
	owner, destOwner := source.owner(), destination.owner()
	project := createProject(source, owner, "Portable guardrail credentials")
	credential := func(name string, value map[string]any) string {
		return source.want(owner, "POST", "/api/v1/guardrail-credentials", map[string]any{"name": name, "project_id": project, "credential": value}, idem(uuid.NewString()), 201)["id"].(string)
	}
	api := credential("evaluator", map[string]any{"api_key": "source-only"})
	network := credential("network", map[string]any{"proxy_username": "source", "proxy_password": "source-only"})
	definition := source.want(owner, "POST", "/api/v1/guardrails", map[string]any{"name": "portable evaluator", "project_id": project, "type": "webhook", "configuration": map[string]any{"endpoint": "https://example.com/guardrail", "credential_ref": api, "content_egress": true, "network": map[string]any{"credential_id": network}}, "timeout_ms": 1000, "on_error": "fail_closed"}, idem(uuid.NewString()), 201)
	source.want(owner, "POST", "/api/v1/guardrails/"+definition["id"].(string)+"/activate", map[string]any{"revision_id": definition["revision_id"]}, withMatch(definition, idem(uuid.NewString())), 204)
	doc := source.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	input := map[string]any{"document": doc, "secret_bindings": map[string]string{"evaluator": `{"api_key":"destination-only"}`, "network": `{"proxy_username":"destination","proxy_password":"destination-only"}`}}
	for range 2 {
		destination.want(destOwner, "POST", "/api/v1/configuration/plan", input, nil, 200)
		destination.want(destOwner, "POST", "/api/v1/configuration/apply", input, idem(uuid.NewString()), 200)
	}
	var count int
	if err := destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.guardrail_credentials").Scan(&count); err != nil || count != 2 {
		t.Fatalf("credentials missing or duplicated: %d %v", count, err)
	}
	var config struct {
		CredentialRef string `json:"credential_ref"`
		Network       struct {
			CredentialID string `json:"credential_id"`
		} `json:"network"`
	}
	var raw []byte
	if err := destination.Pool.QueryRow(t.Context(), "SELECT v.configuration FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.id=g.active_revision_id WHERE g.name='portable evaluator'").Scan(&raw); err != nil || json.Unmarshal(raw, &config) != nil {
		t.Fatalf("missing promoted configuration: %v", err)
	}
	if config.CredentialRef == api || config.Network.CredentialID == network || config.CredentialRef == "evaluator" || config.Network.CredentialID == "network" {
		t.Fatal("promoted guardrail did not bind destination credential IDs")
	}
	destination.refresh()
}
