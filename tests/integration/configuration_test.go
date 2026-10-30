//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/configuration"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
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
	if document["api_version"] != "openllmproxy.dev/config/v2" {
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
	if stagedDraftDetail["content_policy"] != nil {
		t.Fatal("version 2 must use guardrail policies", stagedDraftDetail)
	}
	var stagedRules int
	if err := destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.guardrail_draft_attachments WHERE draft_id=$1", stagedDraft["id"]).Scan(&stagedRules); err != nil || stagedRules != 1 {
		t.Fatal("applied configuration must stage the migrated guardrail policy", stagedRules, err)
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
	// A pending enrollment belongs to the retained slot identity. Staging the
	// same slot must preserve it and advance the collection's concurrency token.
	poolBefore := destination.want(destinationOwner, "GET", "/api/v1/providers/"+providerID+"/credential-slots", nil, nil, 200)
	pendingID := seedPromotionEnrollment(t, destination, providerID, slotID)
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
	poolAfter := destination.want(destinationOwner, "GET", "/api/v1/providers/"+providerID+"/credential-slots", nil, nil, 200)
	if poolAfter["etag"] == poolBefore["etag"] {
		t.Fatal("promotion kept a stale credential collection ETag")
	}
	var pending, secretPresent bool
	if err = destination.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp.grant_enrollments WHERE id=$1),EXISTS(SELECT 1 FROM olp.secrets WHERE id=$1)", pendingID).Scan(&pending, &secretPresent); err != nil {
		t.Fatal(err)
	}
	if !pending || !secretPresent {
		t.Fatal("promotion destroyed enrollment state for a retained slot")
	}
	destination.want(destinationOwner, "DELETE", "/api/v1/providers/"+providerID+"/credential-slots/"+slotID, nil, withMatch(poolBefore, idem("stale-pool-delete")), 412)
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

	// API keys are not portable, so apply keeps a destination slot's own key
	// restriction instead of widening the slot to every key.
	const restrictedKey = "00000000-0000-0000-0000-000000000001"
	if _, err = destination.Pool.Exec(t.Context(), `UPDATE olp.provider_slots SET restrictions=jsonb_set(restrictions,'{allowed_api_keys}',$2::jsonb) WHERE id=$1`, slotID, `["`+restrictedKey+`"]`); err != nil {
		t.Fatal(err)
	}
	clone := func(source map[string]any) map[string]any {
		encoded, _ := json.Marshal(source)
		copied := map[string]any{}
		if err := json.Unmarshal(encoded, &copied); err != nil {
			t.Fatal(err)
		}
		return copied
	}
	extended := clone(updated)
	extendedModels := extended["providers"].([]any)[0].(map[string]any)["models"].([]any)
	extra := clone(extendedModels[0].(map[string]any))
	extra["upstream_model"], extra["display_name"] = "extra-model", "extra-model"
	extended["providers"].([]any)[0].(map[string]any)["models"] = append(extendedModels, extra)
	bindings := map[string]any{bindingKey: vendorSecret}
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": extended, "secret_bindings": bindings}, idem("apply-extra"), 200)
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": updated, "secret_bindings": bindings}, idem("apply-drop-extra"), 200)
	var allowedKeys string
	destination.Pool.QueryRow(t.Context(), "SELECT restrictions->>'allowed_api_keys' FROM olp.provider_slots WHERE id=$1", slotID).Scan(&allowedKeys)
	if allowedKeys != `["`+restrictedKey+`"]` {
		t.Fatal("apply must keep the destination slot's API-key restriction", allowedKeys)
	}
	// The dropped model stays as a disabled row; re-planning must not see it.
	replanned := destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", map[string]any{"document": updated, "secret_bindings": bindings}, nil, 200)
	if !slices.ContainsFunc(replanned["actions"].([]any), func(item any) bool {
		entry := item.(map[string]any)
		return entry["kind"] == "provider" && entry["key"] == "Promoted vendor" && entry["action"] == "noop"
	}) {
		t.Fatal("re-planning after a model was dropped must be a noop", replanned)
	}

	// A provider may name a destination project the document does not declare.
	undeclared := clone(document)
	undeclared["projects"] = []any{}
	undeclared["routes"] = []any{}
	delete(undeclared, "pricing")
	fresh := undeclared["providers"].([]any)[0].(map[string]any)
	fresh["name"], fresh["project"] = "Fresh vendor", "Other"
	freshRef := "Fresh vendor/" + slotName
	for _, slot := range fresh["slots"].([]any) {
		if slot.(map[string]any)["credential_ref"] != nil {
			slot.(map[string]any)["credential_ref"] = "Fresh vendor/" + slot.(map[string]any)["name"].(string)
		}
	}
	undeclared["providers"] = []any{fresh}
	undeclaredBody := map[string]any{"document": undeclared, "secret_bindings": map[string]any{freshRef: vendorSecret}}
	undeclaredPlan := destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", undeclaredBody, nil, 200)
	if len(undeclaredPlan["blockers"].([]any)) != 0 || len(undeclaredPlan["conflicts"].([]any)) != 0 {
		t.Fatal("a destination project must resolve without being declared", undeclaredPlan)
	}
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", undeclaredBody, idem("apply-undeclared"), 200)
	var freshProject, otherProject string
	destination.Pool.QueryRow(t.Context(), "SELECT coalesce(project_id::text,'') FROM olp.providers WHERE name='Fresh vendor'").Scan(&freshProject)
	destination.Pool.QueryRow(t.Context(), "SELECT id::text FROM olp.projects WHERE name='Other'").Scan(&otherProject)
	if freshProject == "" || freshProject != otherProject {
		t.Fatal("apply must stage the provider in the destination project", freshProject, otherProject)
	}
}

// An imported project starts with its creator as manager, as one created
// through the projects API does.

// An unchanged pricing section does not require settings.

func seedPromotionEnrollment(t *testing.T, h *accessHarness, providerID, slotID string) string {
	t.Helper()
	pendingID := access.NewID()
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	var actor string
	if err = tx.QueryRow(t.Context(), "SELECT id::text FROM olp.users WHERE role='owner' LIMIT 1").Scan(&actor); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if _, err = tx.Exec(t.Context(), `INSERT INTO olp.grant_enrollments(id,provider_id,slot_id,plugin_digest,profile_id,started_by,expires_at) VALUES($1,$2,$3,'retained-test-digest','test',$4,$5)`, pendingID, providerID, slotID, actor, expires); err != nil {
		t.Fatal(err)
	}
	if err = h.Server.Keys.Store(t.Context(), tx, h.Server.Installation, pendingID, secrets.GrantEnrollment, []byte(`{"pending":"same slot"}`), &expires); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return pendingID
}

// Exercise the unique positions/default constraints without changing IDs.

func TestCodeConfigurationCrossProjectRoundTrip(t *testing.T) {
	f := newCodingPlanFixture(t, "zai-coding", codeplans.ZAIProfile, "controlled-portability-source-0123456789")
	h, owner := f.h, f.owner
	profile := h.want(owner, "GET", "/api/v1/profile", nil, nil, 200)
	ownerID := profile["id"].(string)
	account := codeDiscoveryResponse[codemode.Account](t, f.accounts[0])
	h.want(owner, "PUT", "/api/v1/code/accounts/"+account.ID, providers.CodeAccountInput{ProjectID: strings.ToUpper(f.project), ProviderID: strings.ToUpper(account.ProviderID), CredentialID: strings.ToUpper(account.CredentialID), Name: account.Name, Enabled: account.Enabled, Models: account.Models}, etagHeader(f.accounts[0]), 200)
	personal := h.want(owner, "POST", "/api/v1/code/pools", access.CodePoolInput{ProjectID: f.project, Name: "Personal portable pool", Kind: "personal", OwnerUserID: &ownerID, AccountIDs: []string{account.ID}, AccountWeights: map[string]int64{account.ID: 3}, APIKeyIDs: []string{f.keyID}}, idem("portable-personal"), 201)
	poolID := personal["id"].(string)
	upperOwner := strings.ToUpper(ownerID)
	h.want(owner, "PUT", "/api/v1/code/pools/"+poolID, access.CodePoolInput{ProjectID: strings.ToUpper(f.project), Name: "Personal portable pool", Kind: "personal", OwnerUserID: &upperOwner, AccountIDs: []string{strings.ToUpper(account.ID)}, AccountWeights: map[string]int64{strings.ToUpper(account.ID): 3}, APIKeyIDs: []string{strings.ToUpper(f.keyID)}}, etagHeader(personal), 200)
	routeID := f.route["id"].(string)
	policy := routes.CodeRouteInput{ProjectID: strings.ToUpper(f.project), Slug: "qualification", PoolID: strings.ToUpper(poolID), Models: account.Models, Enabled: true, ModelRewriting: true, Aliases: map[string]string{"portable": account.Models[0]}, ModelPools: map[string][]string{"portable": account.Models}, InitialRecoveryAttempts: 2, CodexCLIIdentity: true, MessagesToCodex: true, ChatToAntigravity: true, MaxBodyBytes: new(int64(1048576)), Tools: &codemode.ResearchTools{AccountID: strings.ToUpper(account.ID), Services: []string{"search"}}}
	draft := h.want(owner, "PUT", "/api/v1/code/routes/"+routeID, policy, etagHeader(f.route), 200)
	published := h.want(owner, "POST", "/api/v1/code/routes/"+routeID+"/publish", nil, withMatch(draft, idem("portable-publish")), 200)
	unpublished := policy
	unpublished.Enabled = false
	unpublished.CodexCLIIdentity = false
	unpublished.InitialRecoveryAttempts = 0
	h.want(owner, "PUT", "/api/v1/code/routes/"+routeID, unpublished, etagHeader(published), 200)
	daily, monthly := int64(1000), int64(4000)
	h.want(owner, "POST", "/api/v1/code/budgets", access.CodeBudgetInput{ProjectID: f.project, RouteID: &routeID, APIKeyID: &f.keyID, DailyTokens: &daily, MonthlyTokens: &monthly, Enabled: true}, idem("portable-budget"), 201)
	exported := codeDiscoveryResponse[struct {
		Document configuration.Document `json:"document"`
	}](t, h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200))
	encoded, err := json.Marshal(exported.Document.Code)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{f.keys[0], f.key, "credential_id", "ciphertext", "principal", "allowance", "health", "created_by", "revision_id"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("export contains local credential or observation %q", secret)
		}
	}
	sourceCode := exported.Document.Code
	if sourceCode == nil {
		t.Fatal("code configuration missing")
	}
	sourceRoute := sourceCode.Routes[slices.IndexFunc(sourceCode.Routes, func(r configuration.CodeRouteEntry) bool { return r.ID == routeID })]
	if sourceRoute.Published == nil || sourceRoute.Draft.CodexCLIIdentity || !sourceRoute.Published.Policy.CodexCLIIdentity || sourceRoute.Draft.Enabled || !sourceRoute.Published.Policy.Enabled || !sourceRoute.Draft.ChatToAntigravity || !sourceRoute.Published.Policy.ChatToAntigravity {
		t.Fatal("draft and publication were not preserved separately")
	}
	destinationProject := createProject(h, owner, "Portable destination")
	destinationFixture := *f
	destinationFixture.project = destinationProject
	destinationName := "Portable destination provider"
	destinationProvider := destinationFixture.provider(t, destinationName, f.profile)
	destinationCredential := destinationFixture.enroll(t, destinationProvider, "controlled-portability-destination-0123456789")
	slug := "portable-copy"
	destinationKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Destination code key", "project_id": destinationProject, "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem("portable-key"), 201)
	destinationKeyID := destinationKey["id"].(string)
	sourcePool := sourceCode.Pools[slices.IndexFunc(sourceCode.Pools, func(p configuration.CodePoolEntry) bool { return p.ID == poolID })]
	sourceAccount := sourceCode.Accounts[slices.IndexFunc(sourceCode.Accounts, func(a configuration.CodeAccountEntry) bool { return a.ID == account.ID })]
	sourceBudget := sourceCode.Budgets[slices.IndexFunc(sourceCode.Budgets, func(b configuration.CodeBudgetEntry) bool { return b.Route != nil && *b.Route == routeID })]
	copiedAccount := sourceAccount
	copiedAccount.Project = "Portable destination"
	copiedAccount.Provider = destinationName
	copiedPool := sourcePool
	copiedPool.Project = "Portable destination"
	copiedRoute := sourceRoute
	copiedRoute.Project = "Portable destination"
	copiedRoute.Slug = slug
	copiedRoute.Published = &configuration.CodePublication{Policy: sourceRoute.Published.Policy, Connections: map[string]configuration.CodeConnectionEntry{destinationName: sourceRoute.Published.Connections["Coding plan 0"]}}
	copiedBudget := sourceBudget
	copiedBudget.Project = "Portable destination"
	document := configuration.Document{APIVersion: configuration.APIVersion, Projects: []configuration.ProjectEntry{}, Providers: []configuration.ProviderEntry{}, Routes: []configuration.RouteEntry{}, Code: &configuration.CodeConfiguration{Accounts: []configuration.CodeAccountEntry{copiedAccount}, Pools: []configuration.CodePoolEntry{copiedPool}, Routes: []configuration.CodeRouteEntry{copiedRoute}, Budgets: []configuration.CodeBudgetEntry{copiedBudget}}}
	bindings := configuration.CodeBindings{Credentials: map[string]string{sourceAccount.CredentialRef: strings.ToUpper(destinationCredential)}, Users: map[string]string{*sourcePool.OwnerRef: strings.ToUpper(ownerID)}, APIKeys: map[string]string{sourcePool.APIKeyRefs[0]: strings.ToUpper(destinationKeyID)}}
	input := map[string]any{"document": document, "code_bindings": bindings}
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document}, nil, 422)
	wrongCredential := bindings
	wrongCredential.Credentials = map[string]string{sourceAccount.CredentialRef: account.CredentialID}
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document, "code_bindings": wrongCredential}, nil, 422)
	wrongKey := bindings
	wrongKey.APIKeys = map[string]string{sourcePool.APIKeyRefs[0]: f.keyID}
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document, "code_bindings": wrongKey}, nil, 422)
	outsider := h.invite(owner, "portable-outsider@example.test", "developer")
	outsiderProfile := h.want(outsider, "GET", "/api/v1/profile", nil, nil, 200)
	wrongOwner := bindings
	wrongOwner.Users = map[string]string{*sourcePool.OwnerRef: outsiderProfile["id"].(string)}
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document, "code_bindings": wrongOwner}, nil, 422)
	planned := h.want(owner, "POST", "/api/v1/configuration/plan", input, nil, 200)
	if len(planned["blockers"].([]any))+len(planned["conflicts"].([]any)) != 0 {
		t.Fatal("destination plan refused valid bindings", planned)
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", input, idem("portable-apply"), 200)
	replanned := h.want(owner, "POST", "/api/v1/configuration/plan", input, nil, 200)
	for _, raw := range replanned["actions"].([]any) {
		action := raw.(map[string]any)
		if strings.HasPrefix(action["kind"].(string), "code_") && action["action"] != "noop" {
			t.Fatal("second code plan must be unchanged", action)
		}
	}
	destinationExport := codeDiscoveryResponse[struct {
		Document configuration.Document `json:"document"`
	}](t, h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200))
	restored := destinationExport.Document.Code.Routes[slices.IndexFunc(destinationExport.Document.Code.Routes, func(r configuration.CodeRouteEntry) bool { return r.Slug == slug })]
	if restored.ID == sourceRoute.ID || restored.Published == nil || restored.Draft.CodexCLIIdentity || !restored.Published.Policy.CodexCLIIdentity || restored.Draft.Enabled || !restored.Draft.ChatToAntigravity || !restored.Published.Policy.Enabled || restored.Published.Policy.InitialRecoveryAttempts != sourceRoute.Published.Policy.InitialRecoveryAttempts || restored.Published.Policy.Aliases["portable"] != sourceRoute.Published.Policy.Aliases["portable"] || !restored.Published.Policy.MessagesToCodex || !restored.Published.Policy.ChatToAntigravity || restored.Published.Policy.Tools == nil {
		t.Fatal("destination did not retain independent effective and draft policies", restored)
	}
	restoredPools := h.want(owner, "GET", "/api/v1/code/pools?project_id="+destinationProject, nil, nil, 200)
	restoredPool := codeDiscoveryResponse[struct {
		Items []codemode.Pool `json:"items"`
	}](t, restoredPools).Items[0]
	if restoredPool.Kind != sourcePool.Kind || restoredPool.OwnerUserID == nil || *restoredPool.OwnerUserID != ownerID || len(restoredPool.AccountWeights) != 1 || restoredPool.AccountWeights[restoredPool.AccountIDs[0]] != 3 || restoredPool.APIKeyIDs[0] != destinationKeyID {
		t.Fatal("personal pool ownership, membership or weights changed", restoredPool)
	}
	budgetRecords := h.want(owner, "GET", "/api/v1/code/budgets?project_id="+destinationProject, nil, nil, 200)
	budgets := codeDiscoveryResponse[struct {
		Items []access.CodeBudgetInput `json:"items"`
	}](t, budgetRecords).Items
	if len(budgets) != 1 || budgets[0].DailyTokens == nil || *budgets[0].DailyTokens != daily || budgets[0].MonthlyTokens == nil || *budgets[0].MonthlyTokens != monthly || budgets[0].APIKeyID == nil || *budgets[0].APIKeyID != destinationKeyID {
		t.Fatal("budget policy did not roundtrip", budgets)
	}
	h.refresh()

	call := func() (int, []byte) {
		request, requestError := http.NewRequestWithContext(t.Context(), "POST", f.gateway.URL+"/code/"+slug+"/v1/chat/completions", strings.NewReader(`{"model":"portable","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"configuration roundtrip"}]}`))
		if requestError != nil {
			t.Fatal(requestError)
		}
		request.Header.Set("Authorization", "Bearer "+destinationKey["secret"].(string))
		request.Header.Set("Content-Type", "application/json")
		response, responseError := f.gateway.Client().Do(request)
		if responseError != nil {
			t.Fatal(responseError)
		}
		defer response.Body.Close()
		body, bodyError := io.ReadAll(response.Body)
		if bodyError != nil {
			t.Fatal(bodyError)
		}
		return response.StatusCode, body
	}
	blockedStatus, blockedBody := call()
	if blockedStatus != 422 || !strings.Contains(string(blockedBody), "code_token_bound_unavailable") {
		t.Fatalf("restored hard budget did not refuse an unbounded request %d %s", blockedStatus, blockedBody)
	}
	budgetRecord := budgetRecords["items"].([]any)[0].(map[string]any)
	h.want(owner, "PUT", "/api/v1/code/budgets/"+budgetRecord["id"].(string), access.CodeBudgetInput{ProjectID: strings.ToUpper(destinationProject), RouteID: new(strings.ToUpper(restored.ID)), APIKeyID: new(strings.ToUpper(destinationKeyID)), DailyTokens: &daily, MonthlyTokens: &monthly, Enabled: false}, etagHeader(budgetRecord), 200)
	status, responseBody := call()
	if status != 200 || !strings.Contains(string(responseBody), "CONTROLLED_ANSWER") {
		t.Fatalf("restored route response after explicit budget disable %d %s", status, responseBody)
	}
	upstream := f.peer.received()
	if !slices.ContainsFunc(upstream, func(r peerRequest) bool {
		var body struct {
			Model string `json:"model"`
		}
		return json.Unmarshal(r.Body, &body) == nil && body.Model == account.Models[0] && r.Header.Get("Authorization") == "Bearer controlled-portability-destination-0123456789"
	}) {
		t.Fatal("restored route did not use bound destination enrollment and published alias")
	}
}

func TestCodeConfigurationPaidFrozenPolicyRoundTrip(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback-%t", fallback), func(t *testing.T) { codeConfigurationPaidFrozenPolicyRoundTrip(t, fallback) })
	}
}
func codeConfigurationPaidFrozenPolicyRoundTrip(t *testing.T, fallback bool) {
	t.Helper()
	coding := newCodingPlanFixture(t, "zai-coding", codeplans.ZAIProfile, "controlled-paid-portability-source-0123456789")
	account := codeDiscoveryResponse[codemode.Account](t, coding.accounts[0])
	fixture := &codeFixture{h: coding.h, owner: coding.owner, project: coding.project, key: coding.keyID, provider: account.ProviderID, credential: account.CredentialID, account: account.ID, pool: coding.pool["id"].(string), accountRecord: coding.accounts[0], poolRecord: coding.pool, routeRecord: coding.route, route: codeDiscoveryResponse[codemode.Route](t, coding.route)}
	f, paid, slot := publishPaidCodeFixture(t, fixture, true, &routing.Limits{RequestsPerMinute: new(int64(10))})

	if fallback {
		target := codemode.PaidTarget{ProviderID: strings.ToUpper(paid.ID), SlotID: strings.ToUpper(slot.ID)}
		saved := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, routes.CodeRouteInput{ProjectID: f.project, Slug: f.route.Slug, PoolID: f.pool, Models: f.route.Models, Enabled: true, PaidParticipation: true, PaidFallbackTargets: map[string]codemode.PaidTarget{account.Models[0]: target}, InitialRecoveryAttempts: 1}, map[string]string{"If-Match": `"` + f.route.ETag + `"`}, 200)
		published := f.h.want(f.owner, "POST", "/api/v1/code/routes/"+f.route.ID+"/publish", nil, withMatch(saved, idem("portable-fallback-publish")), 200)
		f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+f.route.ID, routes.CodeRouteInput{ProjectID: f.project, Slug: f.route.Slug, PoolID: f.pool, Models: f.route.Models, Enabled: true, PaidParticipation: true, PaidTargets: map[string]codemode.PaidTarget{account.Models[0]: target}}, etagHeader(published), 200)
	}
	exported := f.h.want(f.owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	source := codePublicDecode[configuration.Document](t, exported["document"])

	portableJSON, portableError := json.Marshal(source.Code)
	if portableError != nil {
		t.Fatal(portableError)
	}
	for _, local := range []string{*slot.CredentialID, "subscription_root_id", "paid_bindings", "credential_id", "ciphertext"} {
		if strings.Contains(string(portableJSON), local) {
			t.Fatal("coding export included credentials or runtime linkage")
		}
	}
	sourceRoute := source.Code.Routes[0]

	if fallback && (len(sourceRoute.Published.Policy.PaidFallbackTargets) != 1 || len(sourceRoute.Published.Policy.PaidTargets) != 0 || sourceRoute.Published.Policy.InitialRecoveryAttempts != 1 || len(sourceRoute.Draft.PaidTargets) != 1 || len(sourceRoute.Draft.PaidFallbackTargets) != 0) {
		t.Fatalf("public export changed separate fallback publication and direct draft %+v", sourceRoute)
	}
	frozen := sourceRoute.Published.PaidConnections[paid.Name]
	if len(frozen.SlotKeyRefs[slot.Name]) != 1 || frozen.Provider.Slots[0].Weight != slot.Weight {
		t.Fatal("export lost frozen slot policy")
	}
	currentRevision := access.NewID()
	f.exec(t, `INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,credential_version,source_etag,activated_by) SELECT $2,provider_id,revision+1,name,configuration,jsonb_set(models,'{0,capabilities}',(models->0->'capabilities')||'[{"operation":"generation","surface":"openai","mode":"streaming","source":"certified"}]'::jsonb),jsonb_set(slots,'{0,weight}','7'),credential_version,source_etag,activated_by FROM olp.provider_revisions WHERE id=$1`, paid.RevisionID, currentRevision)
	f.exec(t, `UPDATE olp.providers SET active_revision_id=$2,active_revision=active_revision+1 WHERE id=$1`, paid.ID, currentRevision)

	poolRef := access.NewID()
	pool := configuration.CodePoolEntry{ID: poolRef, Project: source.Code.Pools[0].Project, Name: "Portable paid pool", Kind: source.Code.Pools[0].Kind, Accounts: source.Code.Pools[0].Accounts, APIKeyRefs: source.Code.Pools[0].APIKeyRefs}
	policy := configuration.CodeRoutePolicy{Pool: poolRef, Models: sourceRoute.Draft.Models, Enabled: sourceRoute.Draft.Enabled, PaidParticipation: sourceRoute.Draft.PaidParticipation, PaidTargets: sourceRoute.Draft.PaidTargets, PaidFallbackTargets: sourceRoute.Draft.PaidFallbackTargets, InitialRecoveryAttempts: sourceRoute.Draft.InitialRecoveryAttempts}
	publishedPolicy := configuration.CodeRoutePolicy{Pool: poolRef, Models: sourceRoute.Published.Policy.Models, Enabled: sourceRoute.Published.Policy.Enabled, PaidParticipation: sourceRoute.Published.Policy.PaidParticipation, PaidTargets: sourceRoute.Published.Policy.PaidTargets, PaidFallbackTargets: sourceRoute.Published.Policy.PaidFallbackTargets, InitialRecoveryAttempts: sourceRoute.Published.Policy.InitialRecoveryAttempts}
	route := configuration.CodeRouteEntry{ID: sourceRoute.ID, Project: sourceRoute.Project, Slug: "portable-paid-copy", Draft: policy, Published: &configuration.CodePublication{Policy: publishedPolicy, Connections: sourceRoute.Published.Connections, PaidConnections: sourceRoute.Published.PaidConnections}}
	destinationKey := f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Paid destination key", "project_id": f.project, "scopes": []string{"inference"}, "allowed_routes": []string{route.Slug}}, idem("paid-portable-key"), 201)
	destinationKeyID := destinationKey["id"].(string)
	document := configuration.Document{APIVersion: configuration.APIVersion, Code: &configuration.CodeConfiguration{Accounts: source.Code.Accounts, Pools: []configuration.CodePoolEntry{pool}, Routes: []configuration.CodeRouteEntry{route}, Budgets: source.Code.Budgets}}
	credentials := map[string]string{source.Code.Accounts[0].CredentialRef: f.credential}
	for _, snapshot := range route.Published.PaidConnections {
		for _, portableSlot := range snapshot.Provider.Slots {
			if portableSlot.CredentialRef != nil {
				credentials[*portableSlot.CredentialRef] = *slot.CredentialID
			}
		}
	}
	bindings := configuration.CodeBindings{Credentials: credentials, APIKeys: map[string]string{source.Code.Pools[0].APIKeyRefs[0]: destinationKeyID, frozen.SlotKeyRefs[slot.Name][0]: destinationKeyID}}
	request := map[string]any{"document": document, "code_bindings": bindings}

	f.exec(t, `UPDATE olp.provider_slots SET limits=jsonb_build_object('requests_per_minute',20) WHERE id=$1`, slot.ID)
	f.h.want(f.owner, "POST", "/api/v1/configuration/plan", request, nil, 422)
	f.h.want(f.owner, "POST", "/api/v1/configuration/apply", request, idem("paid-slot-limit-refusal"), 422)
	f.exec(t, `UPDATE olp.provider_slots SET limits=jsonb_build_object('requests_per_minute',10) WHERE id=$1`, slot.ID)
	f.exec(t, `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{options,limits,requests_per_minute}','20')::json WHERE id=$1`, paid.ID)
	f.h.want(f.owner, "POST", "/api/v1/configuration/plan", request, nil, 422)
	f.h.want(f.owner, "POST", "/api/v1/configuration/apply", request, idem("paid-provider-limit-refusal"), 422)
	f.exec(t, `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{options,limits,requests_per_minute}','10')::json WHERE id=$1`, paid.ID)
	plan := f.h.want(f.owner, "POST", "/api/v1/configuration/plan", request, nil, 200)
	if len(plan["conflicts"].([]any))+len(plan["blockers"].([]any)) != 0 {
		t.Fatalf("paid plan was blocked %v", plan)
	}
	f.h.want(f.owner, "POST", "/api/v1/configuration/apply", request, idem("paid-portability"), 200)
	again := f.h.want(f.owner, "POST", "/api/v1/configuration/plan", request, nil, 200)
	for _, action := range again["actions"].([]any) {
		item := action.(map[string]any)
		if strings.HasPrefix(item["kind"].(string), "code_") && item["action"] != "noop" {
			t.Fatalf("paid roundtrip did not converge %v", again)
		}
	}
	result := codePublicDecode[configuration.Document](t, f.h.want(f.owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"])
	copied := slices.IndexFunc(result.Code.Routes, func(entry configuration.CodeRouteEntry) bool { return entry.Slug == route.Slug })
	if copied < 0 {
		t.Fatal("paid route was not copied")
	}
	actual := result.Code.Routes[copied].Published.PaidConnections[paid.Name]
	if actual.Provider.Slots[0].Weight != frozen.Provider.Slots[0].Weight || !reflect.DeepEqual(actual.Provider.Slots[0].Limits, frozen.Provider.Slots[0].Limits) || !reflect.DeepEqual(actual.Provider.Configuration.Options.Limits, frozen.Provider.Configuration.Options.Limits) || len(actual.Provider.Models[0].Capabilities) != len(frozen.Provider.Models[0].Capabilities) || len(actual.SlotKeyRefs[slot.Name]) != 1 {
		t.Fatalf("current paid provider replaced frozen policy %+v", actual)
	}

	if fallback {
		restored := result.Code.Routes[copied]
		target := restored.Published.Policy.PaidFallbackTargets[account.Models[0]]
		if target.Provider != paid.Name || target.Slot != slot.Name || target.CredentialRef == "" || restored.Published.Policy.InitialRecoveryAttempts != 1 || len(restored.Published.Policy.PaidTargets) != 0 || len(restored.Draft.PaidTargets) != 1 || len(restored.Draft.PaidFallbackTargets) != 0 {
			t.Fatalf("roundtrip changed fallback publication or direct draft %+v", restored)
		}
	}
	if !result.Code.Routes[copied].Published.Policy.PaidParticipation {
		t.Fatal("paid authorization was lost")
	}
	f.h.refresh()
	discovery, discoveryError := http.NewRequestWithContext(t.Context(), "GET", coding.gateway.URL+"/code/"+route.Slug+"/v1/models", nil)
	if discoveryError != nil {
		t.Fatal(discoveryError)
	}
	discovery.Header.Set("Authorization", "Bearer "+destinationKey["secret"].(string))
	response, responseError := coding.gateway.Client().Do(discovery)
	if responseError != nil {
		t.Fatal(responseError)
	}
	defer response.Body.Close()
	var catalog struct {
		Data []codemode.Model `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !slices.ContainsFunc(catalog.Data, func(model codemode.Model) bool { return model.ID == account.Models[0] && model.Paid != fallback }) {
		t.Fatalf("restored paid model disappeared from public discovery %d %+v", response.StatusCode, catalog.Data)
	}

}

func TestCodeConfigurationDuplicatePoolsAndBudgetsRoundTrip(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	sourceProject := createProject(h, owner, "Duplicate source")
	createProject(h, owner, "Duplicate destination")
	for index, cap := range []int64{1000, 2000, 1000} {
		h.want(owner, "POST", "/api/v1/code/pools", access.CodePoolInput{ProjectID: sourceProject, Name: "Same pool name", Kind: "shared", AccountIDs: []string{}, APIKeyIDs: []string{}}, idem(fmt.Sprintf("duplicate-pool-%d", index)), 201)
		h.want(owner, "POST", "/api/v1/code/budgets", access.CodeBudgetInput{ProjectID: sourceProject, DailyTokens: &cap, Enabled: true}, idem(fmt.Sprintf("duplicate-budget-%d", index)), 201)
	}
	exported := codePublicDecode[configuration.Document](t, h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"])
	pools := slices.Collect(func(yield func(configuration.CodePoolEntry) bool) {
		for _, pool := range exported.Code.Pools {
			pool.Project = "Duplicate destination"
			if !yield(pool) {
				return
			}
		}
	})
	budgets := slices.Collect(func(yield func(configuration.CodeBudgetEntry) bool) {
		for _, budget := range exported.Code.Budgets {
			budget.Project = "Duplicate destination"
			if !yield(budget) {
				return
			}
		}
	})
	document := configuration.Document{APIVersion: configuration.APIVersion, Code: &configuration.CodeConfiguration{Pools: pools, Budgets: budgets}}
	request := map[string]any{"document": document}
	assertNoops := func(request map[string]any) {
		t.Helper()
		plan := h.want(owner, "POST", "/api/v1/configuration/plan", request, nil, 200)
		if len(plan["conflicts"].([]any))+len(plan["blockers"].([]any)) != 0 {
			t.Fatalf("duplicate policy was blocked %v", plan)
		}
		for _, entry := range plan["actions"].([]any) {
			action := entry.(map[string]any)
			if strings.HasPrefix(action["kind"].(string), "code_") && action["action"] != "noop" {
				t.Fatalf("duplicate configuration did not converge %v", plan)
			}
		}
	}
	h.want(owner, "POST", "/api/v1/configuration/plan", request, nil, 200)
	h.want(owner, "POST", "/api/v1/configuration/apply", request, idem("duplicate-apply"), 200)
	assertNoops(request)
	destinationCode := func() *configuration.CodeConfiguration {
		t.Helper()
		result := codePublicDecode[configuration.Document](t, h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"])
		return &configuration.CodeConfiguration{Pools: slices.Collect(func(yield func(configuration.CodePoolEntry) bool) {
			for _, pool := range result.Code.Pools {
				if pool.Project == "Duplicate destination" && !yield(pool) {
					return
				}
			}
		}), Budgets: slices.Collect(func(yield func(configuration.CodeBudgetEntry) bool) {
			for _, budget := range result.Code.Budgets {
				if budget.Project == "Duplicate destination" && !yield(budget) {
					return
				}
			}
		})}
	}
	initial := destinationCode()
	if len(initial.Pools) != 3 || len(initial.Budgets) != 3 {
		t.Fatalf("duplicate policies were collapsed %+v", initial)
	}
	for _, pool := range initial.Pools {
		if slices.ContainsFunc(exported.Code.Pools, func(source configuration.CodePoolEntry) bool { return source.ID == pool.ID }) {
			t.Fatal("destination pool collided with source")
		}
	}
	for _, budget := range initial.Budgets {
		if slices.ContainsFunc(exported.Code.Budgets, func(source configuration.CodeBudgetEntry) bool { return source.ID == budget.ID }) {
			t.Fatal("destination budget collided with source")
		}
	}
	reexport := map[string]any{"document": configuration.Document{APIVersion: configuration.APIVersion, Code: initial}}
	assertNoops(reexport)
	h.want(owner, "POST", "/api/v1/configuration/apply", reexport, idem("duplicate-reexport"), 200)
	document.Code.Pools[0].Name = "Independently edited pool"
	document.Code.Budgets[0].DailyTokens = new(int64(3500))
	request["document"] = document
	h.want(owner, "POST", "/api/v1/configuration/apply", request, idem("duplicate-edit"), 200)
	assertNoops(request)
	edited := destinationCode()
	if len(edited.Pools) != 3 || len(edited.Budgets) != 3 || !slices.ContainsFunc(edited.Pools, func(pool configuration.CodePoolEntry) bool { return pool.Name == document.Code.Pools[0].Name }) {
		t.Fatalf("editing a duplicate replaced another resource %+v", edited)
	}
	expectedCaps := slices.Sorted(func(yield func(int64) bool) {
		for _, budget := range document.Code.Budgets {
			if !yield(*budget.DailyTokens) {
				return
			}
		}
	})
	actualCaps := slices.Sorted(func(yield func(int64) bool) {
		for _, budget := range edited.Budgets {
			if !yield(*budget.DailyTokens) {
				return
			}
		}
	})
	if !slices.Equal(expectedCaps, actualCaps) {
		t.Fatalf("budget edits affected another duplicate %v %v", expectedCaps, actualCaps)
	}
	ambiguousProject := createProject(h, owner, "Ambiguous destination")
	for index := range 2 {
		h.want(owner, "POST", "/api/v1/code/pools", access.CodePoolInput{ProjectID: ambiguousProject, Name: "Same pool name", Kind: "shared", AccountIDs: []string{}, APIKeyIDs: []string{}}, idem(fmt.Sprintf("ambiguous-pool-%d", index)), 201)
	}
	for index := range 2 {
		h.want(owner, "POST", "/api/v1/code/budgets", access.CodeBudgetInput{ProjectID: ambiguousProject, DailyTokens: new(int64(1000)), Enabled: true}, idem(fmt.Sprintf("ambiguous-budget-%d", index)), 201)
	}
	unrelatedBudget := exported.Code.Budgets[0]
	unrelatedBudget.Project = "Ambiguous destination"
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": configuration.Document{APIVersion: configuration.APIVersion, Code: &configuration.CodeConfiguration{Budgets: []configuration.CodeBudgetEntry{unrelatedBudget}}}}, nil, 422)
	unrelated := exported.Code.Pools[0]
	unrelated.Project = "Ambiguous destination"
	h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": configuration.Document{APIVersion: configuration.APIVersion, Code: &configuration.CodeConfiguration{Pools: []configuration.CodePoolEntry{unrelated}}}}, nil, 422)
}
