//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/database"
)

func TestStrictRouteMigrationRequiresUnseenIdentity(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	provider := newStrictProviderFixture(t, "compatible-chat")
	slug, oldKey := publishStrictProvider(t, h, owner, provider, nil, nil, "legacy")
	route := h.want(owner, "GET", "/api/v3/routes", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	routePath := "/api/v3/routes/" + route["id"].(string)
	before := len(provider.captured())
	sequence := h.Runtime.Release().Sequence

	// Saving a proposed change is safe; publication under a cached identity is not.
	input := fidelityDraft(slug, provider.providerID)
	input["fidelity"] = map[string]any{"mode": "strict"}
	unsafeDraft := h.want(owner, "POST", "/api/v3/route-drafts", input, idem(uuid.NewString()), 201)
	unsafePath := "/api/v3/route-drafts/" + unsafeDraft["id"].(string)
	for _, action := range []string{"validate", "activate"} {
		problem := h.want(owner, "POST", unsafePath+"/"+action, nil, withMatch(unsafeDraft, idem(uuid.NewString())), 422)
		if problemCode(t, problem) != "route_fidelity_migration_required" {
			t.Fatal(problem)
		}
	}
	newSlug := slug + "-strict"
	request := map[string]any{"slug": newSlug}
	headers := withMatch(route, idem(uuid.NewString()))
	h.want(owner, "POST", routePath+"/migration-draft", request, map[string]string{"If-Match": `"stale"`, "Idempotency-Key": uuid.NewString()}, 412)
	h.want(owner, "POST", routePath+"/migration-draft", map[string]any{"slug": slug}, withMatch(route, idem(uuid.NewString())), 422)
	draft := h.want(owner, "POST", routePath+"/migration-draft", request, headers, 201)
	replay := h.want(owner, "POST", routePath+"/migration-draft", request, headers, 201)
	if draft["id"] != replay["id"] || draft["slug"] != newSlug || routeFidelityMode(t, draft["fidelity"]) != "strict" || draft["based_on_revision_id"] != route["latest_revision"].(map[string]any)["id"] {
		t.Fatal("migration did not preserve reviewed source/replay identity", draft, replay)
	}
	// Compare both plans against one source revision and seed without executing
	// either route. The model slug is the only caller identity change.
	shadowSeed := "migration-shadow-v1"
	message := []any{map[string]any{"role": "user", "content": "same native input"}}
	oldPlan := h.list(owner, "POST", "/api/v3/routing/simulate", map[string]any{
		"operation": map[string]any{"operation": "generation", "route": slug, "request": map[string]any{"model": slug, "messages": message}},
		"surface":   "openai", "mode": "unary", "seed": shadowSeed,
	}, nil, 200)
	newPlan := h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/simulate", map[string]any{
		"operation": "generation", "surface": "openai", "mode": "unary", "seed": shadowSeed,
		"request": map[string]any{"model": newSlug, "messages": message},
	}, nil, 200)
	oldDecision := oldPlan[0].(map[string]any)
	newDecision := newPlan["targets"].([]any)[0].(map[string]any)["decision"].(map[string]any)
	for _, field := range []string{"provider_id", "upstream_model", "eligible"} {
		if oldDecision[field] != newDecision[field] {
			t.Fatalf("plan-only migration changed %s: old=%v new=%v", field, oldDecision[field], newDecision[field])
		}
	}
	if oldDecision["eligible"] != true || oldDecision["interaction"].(map[string]any)["fidelity"] != "legacy" || newDecision["interaction"].(map[string]any)["fidelity"] != "strict" || newDecision["interaction"].(map[string]any)["status"] != "admitted" || newPlan["deterministic_seed"] != shadowSeed {
		t.Fatal("migration shadow did not show the legacy/strict contract difference", oldDecision, newDecision)
	}
	h.refresh()
	if len(provider.captured()) != before || h.Runtime.Release().Sequence != sequence {
		t.Fatal("migration draft, shadow plan, or rejected publication performed work")
	}
	path := "/api/v3/route-drafts/" + draft["id"].(string)
	draft = h.want(owner, "POST", path+"/validate", nil, etagHeader(draft), 200)
	activated := h.want(owner, "POST", path+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	newKey := stateKey(t, h, owner, newSlug, false)
	h.refresh()
	for _, target := range []struct{ slug, key string }{{slug, oldKey}, {newSlug, newKey}} {
		status, body, _ := h.gateway("POST", "/v1/chat/completions", target.key, map[string]any{"model": target.slug, "messages": []any{map[string]any{"role": "user", "content": "same native input"}}})
		if status != 200 {
			t.Fatal(status, body)
		}
	}
	if len(provider.captured()) != before+2 {
		t.Fatal("migration duplicated or suppressed provider dispatch")
	}
	// Omitting fidelity from the old revision writer must fail at storage too.
	_, err := h.Pool.Exec(t.Context(), `INSERT INTO olp_go.route_revisions
        (id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,content_policy,routing_policy)
        SELECT $2,route_id,revision+1,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,content_policy,routing_policy
        FROM olp_go.route_revisions WHERE id=$1`, activated["revision_id"], uuid.NewString())
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.ConstraintName != "route_revision_contract_identity" {
		t.Fatal("old writer was not fenced", err)
	}
	_, err = h.Pool.Exec(t.Context(), "UPDATE olp_go.routes SET strict_contract=false WHERE id=$1", activated["route_id"])
	if !errors.As(err, &failure) || failure.ConstraintName != "route_contract_identity" {
		t.Fatal("strict identity could be rewritten", err)
	}
	// The old snapshot writer also loses fields on otherwise unchanged revisions.
	snapshot, err := json.Marshal(h.Runtime.Release().Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var oldSnapshot map[string]json.RawMessage
	if err := json.Unmarshal(snapshot, &oldSnapshot); err != nil {
		t.Fatal(err)
	}
	var oldRoutes map[string]map[string]json.RawMessage
	if err := json.Unmarshal(oldSnapshot["routes"], &oldRoutes); err != nil {
		t.Fatal(err)
	}
	delete(oldRoutes[newSlug], "fidelity")
	oldSnapshot["routes"], _ = json.Marshal(oldRoutes)
	oldBody, _ := json.Marshal(oldSnapshot)
	_, err = h.Pool.Exec(t.Context(), `INSERT INTO olp_go.runtime_releases(id,sequence,sha256,snapshot,created_by,created_at,published_at)
        SELECT $1,sequence+1,sha256,$2,created_by,now(),now() FROM olp_go.runtime_releases ORDER BY sequence DESC LIMIT 1`, uuid.NewString(), oldBody)
	if !errors.As(err, &failure) || failure.ConstraintName != "runtime_route_contract_identity" {
		t.Fatal("old-shaped publication was not fenced", err)
	}
	transformedSlug, _ := publishStrictProvider(t, h, owner, provider, nil, fidelityPolicy("redact", "input"), "transformed")
	var transformedRoute map[string]any
	for _, item := range h.want(owner, "GET", "/api/v3/routes", nil, nil, 200)["items"].([]any) {
		candidate := item.(map[string]any)
		if candidate["slug"] == transformedSlug {
			transformedRoute = candidate
			break
		}
	}
	if transformedRoute == nil {
		t.Fatal("transformed source route not found")
	}
	copy := h.want(owner, "POST", "/api/v3/routes/"+transformedRoute["id"].(string)+"/migration-draft",
		map[string]any{"slug": transformedSlug + "-strict"}, withMatch(transformedRoute, idem(uuid.NewString())), 201)
	if routeFidelityMode(t, copy["fidelity"]) != "strict" || copy["content_policy"] == nil {
		t.Fatal("migration draft silently dropped a source mutation policy", copy)
	}
	path = "/api/v3/route-drafts/" + copy["id"].(string)
	if problemCode(t, h.want(owner, "POST", path+"/validate", nil, etagHeader(copy), 422)) != "fidelity_policy_conflict" {
		t.Fatal("copied incompatible policy was silently accepted")
	}
}

func TestStrictIdentityMigrationRejectsUnsafeHistoryAndRollsBack(t *testing.T) {
	h := newAccessHarness(t)
	fixture := newStrictProviderFixture(t, "compatible-chat")
	owner := h.owner()
	slug, _ := publishStrictProvider(t, h, owner, fixture, nil, nil, "legacy")
	first := h.want(owner, "GET", "/api/v3/routes", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	firstRevision := first["latest_revision"].(map[string]any)["id"].(string)
	draft := h.want(owner, "POST", "/api/v3/route-drafts", fidelityDraft(slug, fixture.providerID), idem(uuid.NewString()), 201)
	second := h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	secondRevision := second["revision_id"].(string)
	if firstRevision == secondRevision {
		t.Fatal("fixture needs two distinct historical revisions")
	}
	// Model an unpublished intermediate writer that once put strictness under
	// an older legacy slug, then apply the new forward migration to that data.
	// The fixture also reverses later 0028 so migration history remains a
	// sequential prefix; production migrations themselves stay forward-only.
	_, err := h.Pool.Exec(t.Context(), `ALTER TABLE olp_go.provider_resources DROP CONSTRAINT provider_resources_interaction_contract_check;
	    ALTER TABLE olp_go.provider_resources DROP CONSTRAINT provider_resources_kind_check;
	    ALTER TABLE olp_go.provider_resources ADD CONSTRAINT provider_resources_kind_check
	        CHECK (kind IN ('file', 'batch', 'response', 'continuation', 'strict_response'));
	    DELETE FROM olp_go.migrations WHERE version='0028_gemini_interaction_resources.sql';
	    DROP TRIGGER check_runtime_route_contracts ON olp_go.runtime_releases;
	    DROP FUNCTION olp_go.check_runtime_route_contracts();
	    DROP TRIGGER check_route_revision_contract ON olp_go.route_revisions;
	    DROP FUNCTION olp_go.check_route_revision_contract();
	    DROP TRIGGER preserve_route_contract_identity ON olp_go.routes;
	    DROP FUNCTION olp_go.preserve_route_contract_identity();
	    ALTER TABLE olp_go.routes DROP COLUMN strict_contract;
	    DELETE FROM olp_go.migrations WHERE version='0027_strict_route_identity.sql'`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp_go.route_revisions SET fidelity='{"mode":"strict"}'::jsonb WHERE id=$1`, secondRevision); err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(t.Context(), h.Pool); err == nil {
		t.Fatal("ambiguous legacy/strict slug history was accepted")
	}
	var schemaRolledBack, historyMissing, laterHistoryMissing bool
	if err = h.Pool.QueryRow(t.Context(), `SELECT NOT EXISTS(SELECT 1 FROM information_schema.columns
	    WHERE table_schema='olp_go' AND table_name='routes' AND column_name='strict_contract'),
	    NOT EXISTS(SELECT 1 FROM olp_go.migrations WHERE version='0027_strict_route_identity.sql'),
	    NOT EXISTS(SELECT 1 FROM olp_go.migrations WHERE version='0028_gemini_interaction_resources.sql')`).Scan(&schemaRolledBack, &historyMissing, &laterHistoryMissing); err != nil || !schemaRolledBack || !historyMissing || !laterHistoryMissing {
		t.Fatal("failed strict identity migration left partial schema/history", err)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp_go.route_revisions SET fidelity=NULL WHERE id=$1`, secondRevision); err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(t.Context(), h.Pool); err != nil {
		t.Fatal("safe historical revision did not migrate after repair", err)
	}
	var sealed bool
	if err = h.Pool.QueryRow(t.Context(), "SELECT NOT strict_contract FROM olp_go.routes WHERE slug=$1", slug).Scan(&sealed); err != nil || !sealed {
		t.Fatal("legacy identity was not retained", err)
	}
}

func TestExplicitNonStrictContractSurvivesOlderWriters(t *testing.T) {
	h := newAccessHarness(t)
	fixture := newStrictProviderFixture(t, "compatible-chat")
	owner := h.owner()
	slug, _ := publishStrictProvider(t, h, owner, fixture, nil, fidelityPolicy("redact", "input"), "transformed")
	route := h.want(owner, "GET", "/api/v3/routes", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	revision := route["latest_revision"].(map[string]any)
	_, err := h.Pool.Exec(t.Context(), `INSERT INTO olp_go.route_revisions
        (id,route_id,revision,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,content_policy,routing_policy)
        SELECT $2,route_id,revision+1,slug,operations,overall_timeout_ms,max_attempts,targets,source_draft_id,activated_by,content_policy,routing_policy
        FROM olp_go.route_revisions WHERE id=$1`, revision["id"], uuid.NewString())
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.ConstraintName != "route_revision_contract_identity" {
		t.Fatal("older revision writer erased explicit transformed contract", err)
	}
	snapshot, err := json.Marshal(h.Runtime.Release().Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var oldSnapshot map[string]json.RawMessage
	if err := json.Unmarshal(snapshot, &oldSnapshot); err != nil {
		t.Fatal(err)
	}
	var oldRoutes map[string]map[string]json.RawMessage
	if err := json.Unmarshal(oldSnapshot["routes"], &oldRoutes); err != nil {
		t.Fatal(err)
	}
	delete(oldRoutes[slug], "fidelity")
	oldSnapshot["routes"], _ = json.Marshal(oldRoutes)
	oldBody, _ := json.Marshal(oldSnapshot)
	_, err = h.Pool.Exec(t.Context(), `INSERT INTO olp_go.runtime_releases(id,sequence,sha256,snapshot,created_by,created_at,published_at)
        SELECT $1,sequence+1,sha256,$2,created_by,now(),now() FROM olp_go.runtime_releases ORDER BY sequence DESC LIMIT 1`, uuid.NewString(), oldBody)
	if !errors.As(err, &failure) || failure.ConstraintName != "runtime_route_contract_identity" {
		t.Fatal("older release writer erased explicit transformed contract", err)
	}
}
