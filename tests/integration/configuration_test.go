//go:build integration

package integration_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConfigurationPromotion(t *testing.T) {
	source := newAccessHarness(t)
	sourceOwner := source.owner()
	sourceUsage := usageMuxFor(source)

	projectID := createProject(source, sourceOwner, "Promote")
	vendor := newVendor(t)
	provider := createScopedProvider(source, sourceOwner, "Promoted vendor", vendor.URL+"/v1", projectID, 201)
	activateScopedProvider(source, sourceOwner, provider)
	draft := source.want(sourceOwner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": "promoted", "project_id": projectID, "fidelity": map[string]any{"mode": "transformed"},
		"operations": []any{"generation"}, "overall_timeout_ms": 30000, "max_attempts": 2,
		"targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 2000}},
		"content_policy": map[string]any{"rules": []any{map[string]any{
			"id": "promo-block", "phase": "input", "pattern": "forbidden-marker", "action": "block",
		}}},
	}, idem("draft-promoted"), 201)
	draftID := draft["id"].(string)
	policy := map[string]any{"allowed_strategies": []any{"weighted"}, "constraints": map[string]any{}, "defaults": map[string]any{}}
	currentPolicy := source.want(sourceOwner, "GET", "/api/v1/routing-policies/route-draft/"+draftID, nil, nil, 200)
	source.want(sourceOwner, "PUT", "/api/v1/routing-policies/route-draft/"+draftID, policy, withMatch(currentPolicy, idem("policy-promoted")), 200)
	draft = source.want(sourceOwner, "GET", "/api/v1/route-drafts/"+draftID, nil, nil, 200)
	activated := source.want(sourceOwner, "POST", "/api/v1/route-drafts/"+draftID+"/activate", nil, withMatch(draft, idem("activate-promoted")), 200)
	routeID := activated["route_id"].(string)

	pricingBody := map[string]any{
		"effective_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"prices":       []any{repPrice("openai_compatible", vendorModel, "generation")},
	}
	if status, _ := source.browserOn(sourceUsage.URL, sourceOwner, "POST", "/api/v1/pricing/revisions", pricingBody, map[string]string{"Idempotency-Key": "pricing-promote"}); status != 201 {
		t.Fatal("pricing revision must be created", status)
	}

	exported := source.want(sourceOwner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	digest, ok := exported["digest"].(string)
	if !ok || len(digest) != 64 {
		t.Fatal("export must return a canonical digest", exported)
	}
	document := exported["document"].(map[string]any)
	encoded, _ := json.Marshal(document)
	for _, forbidden := range []string{vendorSecret, "credential_id", "ciphertext", "actor_", "created_by", "\"api_keys\"", "sessions", "certified_at", "fingerprint"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("export leaks %q: %s", forbidden, encoded)
		}
	}
	if document["api_version"] != "openllmproxy.dev/config/v1" {
		t.Fatal("unexpected api_version", document)
	}
	for _, item := range document["providers"].([]any) {
		for _, slot := range item.(map[string]any)["slots"].([]any) {
			restrictions := slot.(map[string]any)["restrictions"].(map[string]any)
			if keys, ok := restrictions["allowed_api_keys"].([]any); !ok || len(keys) != 0 {
				t.Fatal("export must never carry API-key restrictions", slot)
			}
		}
	}
	document["pricing"].(map[string]any)["effective_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)

	destination := newAccessHarness(t)
	destinationOwner := destination.owner()
	destinationUsage := usageMuxFor(destination)

	keyed := map[string]any{}
	encodedDoc, _ := json.Marshal(document)
	if err := json.Unmarshal(encodedDoc, &keyed); err != nil {
		t.Fatal(err)
	}
	keyed["providers"].([]any)[0].(map[string]any)["slots"].([]any)[0].(map[string]any)["restrictions"].(map[string]any)["allowed_api_keys"] = []any{"00000000-0000-0000-0000-000000000000"}
	if status, out, _ := destination.request(destinationOwner, "POST", "/api/v1/configuration/plan", map[string]any{"document": keyed}, nil); status != 422 || problemCode(t, out) != "non_portable_reference" {
		t.Fatal("imported API-key restrictions must be rejected", status, out)
	}

	planBody := map[string]any{"document": document}
	planned := destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", planBody, nil, 200)
	var bindingKey string
	for _, item := range planned["blockers"].([]any) {
		entry := item.(map[string]any)
		if entry["detail"] == "secret_binding_required" {
			bindingKey = entry["key"].(string)
		}
	}
	if bindingKey == "" {
		t.Fatal("plan must require the credential secret binding", planned)
	}
	rebased := false
	for _, item := range planned["actions"].([]any) {
		entry := item.(map[string]any)
		rebased = rebased || entry["kind"] == "pricing" && entry["detail"] == "effective_at_rebased"
	}
	if !rebased {
		t.Fatal("plan must rebase a past pricing effective time", planned)
	}
	if len(planned["conflicts"].([]any)) != 0 {
		t.Fatal("fresh destination must not conflict", planned)
	}
	if status, out, _ := destination.request(destinationOwner, "POST", "/api/v1/configuration/apply", planBody, idem("apply-no-secret")); status != 409 || problemCode(t, out) != "configuration_not_applicable" {
		t.Fatal("apply without required secrets must refuse", status, out)
	}

	applyStart := time.Now()
	applyBody := map[string]any{"document": document, "secret_bindings": map[string]any{bindingKey: vendorSecret}}
	applied := destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", applyBody, idem("apply-promote"), 200)
	if len(applied["conflicts"].([]any)) != 0 || len(applied["blockers"].([]any)) != 0 {
		t.Fatal("apply must stage cleanly", applied)
	}
	appliedEncoded, _ := json.Marshal(applied)
	if strings.Contains(string(appliedEncoded), vendorSecret) {
		t.Fatal("apply response must never echo secrets")
	}

	projects := destination.want(destinationOwner, "GET", "/api/v1/projects", nil, nil, 200)["items"].([]any)
	var promotedProject map[string]any
	for _, item := range projects {
		if item.(map[string]any)["name"] == "Promote" {
			promotedProject = item.(map[string]any)
		}
	}
	if promotedProject == nil {
		t.Fatal("project must be created", projects)
	}

	providers := destination.want(destinationOwner, "GET", "/api/v1/providers", nil, nil, 200)["items"].([]any)
	var promotedProvider map[string]any
	for _, item := range providers {
		entry := item.(map[string]any)
		if entry["name"] == "Promoted vendor" {
			promotedProvider = entry
		}
	}
	if promotedProvider == nil || promotedProvider["state"] != "draft" {
		t.Fatal("provider must be staged as a draft", providers)
	}
	providerID := promotedProvider["id"].(string)
	models := destination.want(destinationOwner, "GET", "/api/v1/providers/"+providerID+"/models", nil, nil, 200)["items"].([]any)
	if len(models) == 0 {
		t.Fatal("staged provider must declare models", models)
	}
	for _, item := range models {
		for _, capability := range item.(map[string]any)["capabilities"].([]any) {
			entry := capability.(map[string]any)
			if entry["source"] != "declared" || entry["certified_at"] != nil {
				t.Fatal("imported capabilities must stay declared", entry)
			}
		}
	}

	drafts := destination.want(destinationOwner, "GET", "/api/v1/route-drafts", nil, nil, 200)["items"].([]any)
	var stagedDraft map[string]any
	for _, item := range drafts {
		if item.(map[string]any)["slug"] == "promoted" {
			stagedDraft = item.(map[string]any)
		}
	}
	if stagedDraft == nil {
		t.Fatal("route must be staged as a draft", drafts)
	}
	for _, item := range destination.want(destinationOwner, "GET", "/api/v1/routes", nil, nil, 200)["items"].([]any) {
		if item.(map[string]any)["slug"] == "promoted" {
			t.Fatal("apply must never activate routes", item)
		}
	}
	stagedPolicy := destination.want(destinationOwner, "GET", "/api/v1/routing-policies/route-draft/"+stagedDraft["id"].(string), nil, nil, 200)
	if stagedPolicy["policy"].(map[string]any)["allowed_strategies"].([]any)[0] != "weighted" {
		t.Fatal("routing policy must be staged on the draft", stagedPolicy)
	}
	stagedDraftDetail := destination.want(destinationOwner, "GET", "/api/v1/route-drafts/"+stagedDraft["id"].(string), nil, nil, 200)
	stagedRules, ok := stagedDraftDetail["content_policy"].(map[string]any)["rules"].([]any)
	if !ok || len(stagedRules) != 1 || stagedRules[0].(map[string]any)["id"] != "promo-block" {
		t.Fatal("applied configuration must preserve the content policy", stagedDraftDetail)
	}

	status, revisions := destination.browserOn(destinationUsage.URL, destinationOwner, "GET", "/api/v1/pricing/revisions", nil, nil)
	if status != 200 || len(revisions["items"].([]any)) != 1 {
		t.Fatal("pricing revision must be created", status, revisions)
	}
	revisionEffective, err := time.Parse(time.RFC3339, revisions["items"].([]any)[0].(map[string]any)["effective_at"].(string))
	if err != nil || revisionEffective.Before(applyStart.Add(-time.Second)) {
		t.Fatal("a rebased revision must take effect at apply time", revisions)
	}

	var secretLeak int
	destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.replays WHERE fingerprint LIKE '%'||$1||'%'", vendorSecret).Scan(&secretLeak)
	if secretLeak != 0 {
		t.Fatal("replay fingerprints must never contain secrets")
	}
	destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit_events WHERE detail::text LIKE '%'||$1||'%' OR resource_id LIKE '%'||$1||'%'", vendorSecret).Scan(&secretLeak)
	if secretLeak != 0 {
		t.Fatal("audit must never contain secrets")
	}
	audits := destination.want(destinationOwner, "GET", "/api/v1/audit", nil, nil, 200)["items"].([]any)
	found := false
	for _, item := range audits {
		entry := item.(map[string]any)
		if entry["action"] == "configuration.apply" && entry["resource_id"] == applied["digest"] {
			found = true
		}
	}
	if !found {
		t.Fatal("configuration.apply must be audited with the digest", audits)
	}

	replayed := destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", applyBody, idem("apply-promote"), 200)
	if replayed["digest"] != applied["digest"] {
		t.Fatal("replay must return the recorded result", replayed)
	}
	providerBefore := destination.want(destinationOwner, "GET", "/api/v1/providers/"+providerID, nil, nil, 200)
	draftBefore := destination.want(destinationOwner, "GET", "/api/v1/route-drafts/"+stagedDraft["id"].(string), nil, nil, 200)
	var slotsETagBefore string
	destination.Pool.QueryRow(t.Context(), "SELECT slots_etag::text FROM olp.providers WHERE id=$1", providerID).Scan(&slotsETagBefore)
	again := destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", applyBody, idem("apply-again"), 200)
	if len(again["conflicts"].([]any)) != 0 || len(again["blockers"].([]any)) != 0 {
		t.Fatal("a repeated apply must be an idempotent noop", again)
	}
	noop := map[string]bool{}
	for _, item := range again["actions"].([]any) {
		entry := item.(map[string]any)
		if entry["action"] == "noop" {
			noop[entry["kind"].(string)+"/"+entry["key"].(string)] = true
		}
	}
	if !noop["provider/Promoted vendor"] || !noop["route/promoted"] || !noop["pricing/pricing"] {
		t.Fatal("a repeated apply must report noop actions", again)
	}
	providerAfter := destination.want(destinationOwner, "GET", "/api/v1/providers/"+providerID, nil, nil, 200)
	draftAfter := destination.want(destinationOwner, "GET", "/api/v1/route-drafts/"+stagedDraft["id"].(string), nil, nil, 200)
	var slotsETagAfter string
	destination.Pool.QueryRow(t.Context(), "SELECT slots_etag::text FROM olp.providers WHERE id=$1", providerID).Scan(&slotsETagAfter)
	if providerAfter["etag"] != providerBefore["etag"] || draftAfter["etag"] != draftBefore["etag"] || slotsETagAfter != slotsETagBefore {
		t.Fatal("a repeated apply must not rotate provider or draft etags")
	}

	stale := destination.want(destinationOwner, "GET", "/api/v1/configuration/export", nil, nil, 200)["digest"].(string)
	conflictBody := map[string]any{"document": document, "expected_digest": "stale-digest", "secret_bindings": map[string]any{bindingKey: vendorSecret}}
	if status, out, _ := destination.request(destinationOwner, "POST", "/api/v1/configuration/apply", conflictBody, idem("apply-stale")); status != 409 || problemCode(t, out) != "configuration_not_applicable" {
		t.Fatal("a stale expected digest must refuse", status, out)
	}
	conflictPlan := destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", conflictBody, nil, 200)
	found = false
	for _, item := range conflictPlan["conflicts"].([]any) {
		if item.(map[string]any)["detail"] == "configuration_changed" {
			found = true
		}
	}
	if !found {
		t.Fatal("plan must report configuration_changed", conflictPlan)
	}
	current := map[string]any{"document": document, "expected_digest": stale, "secret_bindings": map[string]any{bindingKey: vendorSecret}}
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", current, idem("apply-current"), 200)

	changed := map[string]any{}
	if err = json.Unmarshal(encodedDoc, &changed); err != nil {
		t.Fatal(err)
	}
	changed["providers"].([]any)[0].(map[string]any)["configuration"].(map[string]any)["kind"] = "openai"
	changed["providers"].([]any)[0].(map[string]any)["configuration"].(map[string]any)["options"].(map[string]any)["vendor_id"] = "openai"
	kindConflict := map[string]any{"document": changed, "secret_bindings": map[string]any{bindingKey: vendorSecret}}
	if status, out, _ := destination.request(destinationOwner, "POST", "/api/v1/configuration/apply", kindConflict, idem("apply-kind")); status != 409 || problemCode(t, out) != "configuration_not_applicable" {
		t.Fatal("a provider kind change must refuse", status, out)
	}

	createProject(destination, destinationOwner, "Other")
	mismatched := map[string]any{}
	if err = json.Unmarshal(encodedDoc, &mismatched); err != nil {
		t.Fatal(err)
	}
	mismatched["providers"].([]any)[0].(map[string]any)["project"] = "Other"
	mismatched["routes"].([]any)[0].(map[string]any)["project"] = "Other"
	mismatchBody := map[string]any{"document": mismatched, "secret_bindings": map[string]any{bindingKey: vendorSecret}}
	mismatchPlan := destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", mismatchBody, nil, 200)
	found = false
	for _, item := range mismatchPlan["conflicts"].([]any) {
		found = found || item.(map[string]any)["detail"] == "provider_project_mismatch"
	}
	if !found {
		t.Fatal("plan must report provider_project_mismatch", mismatchPlan)
	}
	if status, out, _ := destination.request(destinationOwner, "POST", "/api/v1/configuration/apply", mismatchBody, idem("apply-mismatch")); status != 409 || problemCode(t, out) != "configuration_not_applicable" {
		t.Fatal("a provider project change must refuse", status, out)
	}

	allToken := destination.want(destinationOwner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "promotion", "scopes": []any{"read", "configure"},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, idem("token-promotion"), 201)["secret"].(string)
	if status, _ := destination.machine(allToken, "GET", "/api/v1/configuration/export", nil, nil); status != 200 {
		t.Fatal("all-project tokens must export", status)
	}
	if status, _ := destination.machine(allToken, "POST", "/api/v1/configuration/plan", planBody, nil); status != 200 {
		t.Fatal("all-project tokens must plan", status)
	}
	if status, _ := destination.machine(allToken, "POST", "/api/v1/configuration/apply", applyBody, idem("machine-apply")); status != 200 {
		t.Fatal("all-project tokens must apply", status)
	}
	scopedToken := destination.want(destinationOwner, "POST", "/api/v1/management-tokens", map[string]any{
		"name": "scoped", "scopes": []any{"read", "configure"}, "project_ids": []any{promotedProject["id"]},
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, idem("token-scoped"), 201)["secret"].(string)
	for _, attempt := range []struct{ method, path string }{
		{"GET", "/api/v1/configuration/export"},
		{"POST", "/api/v1/configuration/plan"},
		{"POST", "/api/v1/configuration/apply"},
	} {
		if status, _ := destination.machine(scopedToken, attempt.method, attempt.path, planBody, idem("scoped"+attempt.path)); status != 403 {
			t.Fatalf("project-scoped tokens must be denied %s %s, got %d", attempt.method, attempt.path, status)
		}
	}

	promoted := destination.want(destinationOwner, "GET", "/api/v1/providers/"+providerID, nil, nil, 200)
	activateScopedProvider(destination, destinationOwner, promoted)
	stagedDetail := destination.want(destinationOwner, "GET", "/api/v1/route-drafts/"+stagedDraft["id"].(string), nil, nil, 200)
	stagedActivation := destination.want(destinationOwner, "POST", "/api/v1/route-drafts/"+stagedDraft["id"].(string)+"/activate", nil, withMatch(stagedDetail, idem("activate-staged")), 200)
	stagedRouteID, _ := stagedActivation["route_id"].(string)
	if stagedRouteID == "" || stagedRouteID == routeID {
		t.Fatal("local activation must publish the staged route", stagedActivation)
	}

	slotName := strings.TrimPrefix(bindingKey, "Promoted vendor/")
	var slotID, modelID string
	destination.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.provider_slots WHERE provider_id=$1 AND name=$2", providerID, slotName).Scan(&slotID)
	destination.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.provider_models WHERE provider_id=$1 AND upstream_model=$2", providerID, vendorModel).Scan(&modelID)
	if slotID == "" || modelID == "" {
		t.Fatal("staged slot and model must exist", slotID, modelID)
	}
	updated := map[string]any{}
	if err = json.Unmarshal(encodedDoc, &updated); err != nil {
		t.Fatal(err)
	}
	updated["providers"].([]any)[0].(map[string]any)["models"].([]any)[0].(map[string]any)["display_name"] = "Renamed model"
	updatedApply := destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": updated, "secret_bindings": map[string]any{bindingKey: vendorSecret}}, idem("apply-update"), 200)
	if len(updatedApply["conflicts"].([]any)) != 0 || len(updatedApply["blockers"].([]any)) != 0 {
		t.Fatal("applying a changed artifact to an active provider must stage cleanly", updatedApply)
	}
	destination.want(destinationOwner, "GET", "/api/v1/routes/"+stagedRouteID, nil, nil, 200)
	var preservedSlot, preservedModel string
	destination.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.provider_slots WHERE provider_id=$1 AND name=$2", providerID, slotName).Scan(&preservedSlot)
	destination.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.provider_models WHERE provider_id=$1 AND upstream_model=$2", providerID, vendorModel).Scan(&preservedModel)
	if preservedSlot != slotID {
		t.Fatal("apply must preserve a same-named slot UUID", slotID, preservedSlot)
	}
	if preservedModel != modelID {
		t.Fatal("apply must preserve the provider model UUID used by the published route", modelID, preservedModel)
	}
	models = destination.want(destinationOwner, "GET", "/api/v1/providers/"+providerID+"/models", nil, nil, 200)["items"].([]any)
	found = false
	for _, item := range models {
		entry := item.(map[string]any)
		if entry["upstream_model"] == vendorModel {
			found = entry["display_name"] == "Renamed model"
			for _, capability := range entry["capabilities"].([]any) {
				if capability.(map[string]any)["source"] != "declared" {
					t.Fatal("applied capabilities must stay declared", capability)
				}
			}
		}
	}
	if !found {
		t.Fatal("the changed artifact must update draft models", models)
	}
}

func TestConfigurationPricingRequiresSettings(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, configure := createToken(h, owner, "configure-only", []string{"configure"})
	_, settings := createToken(h, owner, "configure-settings", []string{"configure", "settings"})
	doc := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	doc["projects"] = []any{map[string]any{"name": "imported"}}
	body := map[string]any{"document": doc}
	h.machineWant(configure, "POST", "/api/v1/configuration/apply", body, idem("no-pricing"), 200)
	doc["projects"] = []any{map[string]any{"name": "must-rollback"}}
	doc["pricing"] = map[string]any{"effective_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"prices": []any{repPrice("openai_compatible", vendorModel, "generation")}}
	h.machineWant(configure, "POST", "/api/v1/configuration/apply", body, idem("denied-pricing"), 403)
	var count int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.projects WHERE name='must-rollback'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected import mutated projects: count=%d err=%v", count, err)
	}
	h.machineWant(settings, "POST", "/api/v1/configuration/apply", body, idem("authorized-pricing"), 200)
	// An unchanged pricing section does not require settings.
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"]
	h.machineWant(configure, "POST", "/api/v1/configuration/apply", map[string]any{"document": exported}, idem("unchanged-pricing"), 200)
}
