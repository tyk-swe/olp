//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

func pendingGeminiGeneration(t *testing.T, h *accessHarness, slug, key string) (string, string, string) {
	t.Helper()
	response, raw := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/interactions", key,
		[]byte(fmt.Sprintf(`{"model":%q,"input":"durable accounting","background":true}`, slug)))
	var accepted struct{ ID, Status string }
	if response.StatusCode != http.StatusOK || json.Unmarshal(raw, &accepted) != nil || accepted.Status != "in_progress" {
		t.Fatalf("pending generation: %d %s", response.StatusCode, raw)
	}
	var original, owner string
	err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage'->>'request_id',api_key_id::text
 FROM olp.provider_resources WHERE id=$1`, strings.ReplaceAll(strings.TrimPrefix(accepted.ID, "interaction_"), "_", "")).Scan(&original, &owner)
	if err != nil || original == "" {
		t.Fatalf("accepted generation has no durable original request: %q %v", original, err)
	}
	return accepted.ID, original, owner
}

func assertGeminiOriginalUsage(t *testing.T, h *accessHarness, original string, complete bool, input, output int64) {
	t.Helper()
	var count int
	var observed, settled bool
	var in, out int64
	err := h.Pool.QueryRow(t.Context(), `SELECT count(*),bool_and(usage_observed),bool_and(usage_complete),
 COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0) FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&count, &observed, &settled, &in, &out)
	if err != nil || count != 1 || settled != complete || observed != (complete || input > 0 || output > 0) || in != input || out != output {
		t.Fatalf("original usage count=%d observed=%v complete=%v tokens=%d/%d err=%v", count, observed, settled, in, out, err)
	}
}

func TestGeminiBackgroundAccountingSurvivesRestartAndTerminalObservers(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	provider.terminalUsage.Store(true)
	slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	for _, terminal := range []string{"get", "stream", "cancel"} {
		t.Run(terminal, func(t *testing.T) {
			local, original, owner := pendingGeminiGeneration(t, h, slug, key)
			attempt := sink.last().Attempts[0]
			if !attempt.GenerationUsageDeferred || attempt.UsageComplete || !attempt.BillingUncertain || attempt.Interaction == nil || attempt.Interaction.UpstreamState != "accepted" {
				t.Fatalf("native pending generation falsely completed: %+v", attempt)
			}
			if err := h.Gateway.Resources.CheckPendingCostOwner(t.Context(), owner); !errors.Is(err, resources.ErrPendingUsage) {
				t.Fatalf("pending owner was not quarantined: %v", err)
			}
			restarted := newAccessHarnessOn(t, h.Pool, h.DBURL)
			restarted.refresh()
			path, method := "/gemini/v1beta/interactions/"+local, http.MethodGet
			if terminal == "stream" {
				path += "?stream=true"
			}
			if terminal == "cancel" {
				path += "/cancel"
				method = http.MethodPost
			}
			for range 2 {
				response, raw := geminiPublic(t, restarted, method, path, key, nil)
				if response.StatusCode != http.StatusOK {
					t.Fatalf("terminal observer: %d %s", response.StatusCode, raw)
				}
			}
			assertGeminiOriginalUsage(t, h, original, true, 5, 4)
			if err := restarted.Gateway.Resources.CheckPendingCostOwner(t.Context(), owner); err != nil {
				t.Fatalf("settled owner stayed quarantined: %v", err)
			}
		})
	}
}

func TestGeminiPendingDeletionAndExpiryPreserveBillingUncertainty(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
	for _, terminal := range []string{"delete", "expiry", "partial-delete", "partial-expiry"} {
		t.Run(terminal, func(t *testing.T) {
			provider.partialUsage.Store(strings.HasPrefix(terminal, "partial-"))
			local, original, owner := pendingGeminiGeneration(t, h, slug, key)
			if strings.HasSuffix(terminal, "delete") {
				response, raw := geminiPublic(t, h, http.MethodDelete, "/gemini/v1beta/interactions/"+local, key, nil)
				if response.StatusCode != http.StatusOK {
					t.Fatalf("delete: %d %s", response.StatusCode, raw)
				}
			} else {
				id := strings.TrimPrefix(local, "interaction_")
				if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET expires_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if _, err := h.Gateway.Resources.Expire(t.Context(), time.Now()); err != nil {
						t.Fatal(err)
					}
				}
			}
			input, output := int64(0), int64(0)
			if strings.HasPrefix(terminal, "partial-") {
				input, output = 3, 2
			}
			assertGeminiOriginalUsage(t, h, original, false, input, output)
			if err := h.Gateway.Resources.CheckPendingCostOwner(t.Context(), owner); err != nil {
				t.Fatalf("removed owner stayed pending: %v", err)
			}
			var charge string
			if err := h.Pool.QueryRow(t.Context(), `SELECT charge_status FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&charge); err != nil || charge != "billing_uncertain" {
				t.Fatalf("removed generation became free: %q %v", charge, err)
			}
		})
	}
}

func TestDeferredExpiryPreservesSubmissionTimingAfterThirtyDays(t *testing.T) {
	h := newAccessHarness(t)
	provider := newOpenAIFixture(t, "")
	owner, _, slug, _ := provisionOpenAI(t, h, provider.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
	provider.resps["resp-up-1"]["status"] = "in_progress"
	provider.resps["resp-up-1"]["usage"] = nil
	provider.resps["resp-up-1"]["expires_at"] = time.Now().Add(30 * 24 * time.Hour).Unix()
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	status, response, _ := h.gateway(http.MethodPost, "/v1/responses", key, map[string]any{"model": slug, "input": "long-lived generation", "background": true, "store": true})
	if status != http.StatusOK {
		t.Fatalf("background Response: %d %v", status, response)
	}
	local := response["id"].(string)
	id := strings.TrimPrefix(local, "response_")
	var raw []byte
	var original string
	if err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage',metadata->'pending_usage'->>'request_id' FROM olp.provider_resources WHERE id=$1`, id).Scan(&raw, &original); err != nil {
		t.Fatal(err)
	}
	template, err := usage.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	age := -31 * 24 * time.Hour
	template.RequestStartedAt = template.RequestStartedAt.Add(age)
	template.RequestCompletedAt = template.RequestCompletedAt.Add(age)
	template.ObservedAt = template.ObservedAt.Add(age)
	for i := range template.Attempts {
		template.Attempts[i].StartedAt = template.Attempts[i].StartedAt.Add(age)
		template.Attempts[i].CompletedAt = template.Attempts[i].CompletedAt.Add(age)
	}
	raw, err = usage.Encode(template)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage}',$2::jsonb),created_at=$3::timestamptz,expires_at=$3::timestamptz+interval '30 days' WHERE id=$1`, id, raw, template.RequestStartedAt); err != nil {
		t.Fatal(err)
	}
	observed := time.Now()
	if count, err := h.Gateway.Resources.Expire(t.Context(), observed); err != nil || count != 1 {
		t.Fatalf("thirty-day owner remained poison: removed=%d err=%v", count, err)
	}
	var started, completed, seen time.Time
	var latency int64
	if err = h.Pool.QueryRow(t.Context(), `SELECT r.started_at,r.completed_at,f.observed_at,r.total_latency_ms FROM olp.requests r JOIN olp.attempt_usage_facts f ON f.request_id=r.id AND f.request_started_at=r.started_at WHERE r.id=$1`, original).Scan(&started, &completed, &seen, &latency); err != nil {
		t.Fatal(err)
	}
	if abs := completed.Sub(template.RequestCompletedAt); abs < -time.Microsecond || abs > time.Microsecond || latency != template.LatencyMS || !seen.After(template.RequestCompletedAt.Add(30*24*time.Hour)) {
		t.Fatalf("deferred observation rewrote original exchange: start=%s end=%s observed=%s latency=%d", started, completed, seen, latency)
	}
	assertGeminiOriginalUsage(t, h, original, false, 0, 0)
}

func TestGeminiBackgroundRefusesBudgetsAndPendingWorkBlocksNewBudgets(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	session := h.owner()
	slug, key, _ := provisionGeminiLifecycle(t, h, session, "gemini-interactions", provider)
	local, _, owner := pendingGeminiGeneration(t, h, slug, key)
	detail := h.want(session, "GET", "/api/v1/api-keys/"+owner, nil, nil, 200)
	h.want(session, "PATCH", "/api/v1/api-keys/"+owner, map[string]any{"daily_cost_limit": "1"}, etagHeader(detail), 200)
	h.refresh()
	h.Gateway.Admission = gateway.NewAdmission(nil, nil, slog.Default())
	h.Gateway.Admission.PendingCostAccountingReady = h.Gateway.Resources.CheckPendingCostOwner
	before := len(provider.captured())
	for _, request := range []struct {
		background bool
		status     int
		code       string
	}{
		{true, http.StatusBadRequest, "unbounded_work_budget"}, {false, http.StatusServiceUnavailable, "cost_accounting_incomplete"},
	} {
		response, raw := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/interactions", key,
			[]byte(fmt.Sprintf(`{"model":%q,"input":"new generation","background":%v}`, slug, request.background)))
		if response.StatusCode != request.status || !strings.Contains(string(raw), request.code) {
			t.Fatalf("budget admission(background=%v): %d %s", request.background, response.StatusCode, raw)
		}
	}
	if len(provider.captured()) != before {
		t.Fatal("unbounded or pending-budget generation reached the provider")
	}
	group := h.want(session, "POST", "/api/v1/budget-groups", map[string]any{"name": "New shared budget", "daily_cost_limit": "1"}, idem("new-shared-budget"), 201)
	detail = h.want(session, "GET", "/api/v1/api-keys/"+owner, nil, nil, 200)
	h.want(session, "PATCH", "/api/v1/api-keys/"+owner, map[string]any{"budget_group_id": group["id"]}, etagHeader(detail), 200)
	global := h.want(session, "GET", "/api/v1/budgets/installation", nil, nil, 200)
	h.want(session, "PUT", "/api/v1/budgets/installation", map[string]any{"policy": map[string]any{"daily_cost_limit": "1"}}, etagHeader(global), 200)
	h.refresh()
	response, raw := geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/interactions/"+local, key, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("new budget prevented terminal housekeeping: %d %s", response.StatusCode, raw)
	}
}

func TestPendingGenerationProjectsEveryBudgetOwnerAndBackfills(t *testing.T) {
	h := newAccessHarness(t)
	session := h.owner()
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, session, "gemini-interactions", provider)
	local, _, owner := pendingGeminiGeneration(t, h, slug, key)
	org := h.want(session, "POST", "/api/v1/organizations", map[string]any{"name": "Pending organization"}, idem("pending-org"), 201)["id"].(string)
	project := h.want(session, "POST", "/api/v1/organizations/"+org+"/projects", map[string]any{"name": "Pending project"}, idem("pending-project"), 201)["id"].(string)
	group := h.want(session, "POST", "/api/v1/budget-groups", map[string]any{"name": "Pending group", "daily_cost_limit": "1"}, idem("pending-group"), 201)["id"].(string)
	var raw []byte
	var providerID, slotID, pinnedRouteID, createdBy, installation, routeRevision string
	id := strings.TrimPrefix(local, "interaction_")
	if err := h.Pool.QueryRow(t.Context(), `SELECT pr.metadata->'pending_usage',pr.provider_id::text,pr.slot_id::text,r.id::text,pr.route_revision_id::text,k.created_by::text,(SELECT id::text FROM olp.installation)
 FROM olp.provider_resources pr JOIN olp.routes r ON r.slug=pr.route_slug JOIN olp.api_keys k ON k.id=pr.api_key_id WHERE pr.id=$1`, id).Scan(&raw, &providerID, &slotID, &pinnedRouteID, &routeRevision, &createdBy, &installation); err != nil {
		t.Fatal(err)
	}
	var event usage.Event
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	named := "named-pending"
	namedID, recordedID := uuid.NewString(), uuid.NewString()
	digest := strings.Repeat("a", 64)
	event.RouteSlug = named
	event.BudgetGroupID = &group
	event.EndUserDigest = digest
	event.Attribution = map[string]string{"team": "infra"}
	event.Attempts[0].Routing.Leg = &usage.RouteLeg{Route: slug, RevisionID: routeRevision, Via: usage.ViaFallback}
	event.Attempts[0].Routing.Budgets = []string{recordedID}
	raw, _ = json.Marshal(event)
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.routes(id,slug,created_by,latest_revision,latest_revision_id,etag,project_id) SELECT $1,$2,created_by,latest_revision,latest_revision_id,$1,$3 FROM olp.routes WHERE slug=$4`, []any{namedID, named, project, slug}},
		{`UPDATE olp.api_keys SET project_id=$1 WHERE id=$2`, []any{project, owner}},
		{`UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage}',$2::jsonb) WHERE id=$1`, []any{id, raw}},
	} {
		if _, err = tx.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, subject := range []struct{ level, id string }{{"installation", installation}, {"organization", org}, {"project", project}} {
		if err = limits.EnsureAggregateBudget(t.Context(), tx, subject.level, subject.id); err != nil {
			t.Fatal(err)
		}
	}
	if err = limits.EnsureKeyRouteBudget(t.Context(), tx, owner, named); err != nil {
		t.Fatal(err)
	}
	if err = limits.EnsureAttributionBudget(t.Context(), tx, project, "team", "infra"); err != nil {
		t.Fatal(err)
	}
	endKey, endProject := limits.EndUserKeyID(owner, digest), limits.EndUserProjectID(project, digest)
	if _, err = tx.Exec(t.Context(), `INSERT INTO olp.end_user_accounts(id,api_key_id,project_id,end_user_digest) VALUES($1,$2,NULL,$5),($3,NULL,$4,$5)`, endKey, owner, endProject, project, digest); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	owners := []string{owner, group, providerID, slotID, pinnedRouteID, namedID, recordedID, endKey, endProject,
		limits.AggregateBudgetID("installation", installation), limits.AggregateBudgetID("organization", org), limits.AggregateBudgetID("project", project), limits.AttributionBudgetID(project, "team", "infra"), limits.KeyRouteBudgetID(owner, named)}
	check := func(pending bool) {
		t.Helper()
		for _, subject := range owners {
			err := h.Gateway.Resources.CheckPendingCostOwner(t.Context(), subject)
			if pending && !errors.Is(err, resources.ErrPendingUsage) || !pending && err != nil {
				t.Fatalf("subject %s pending=%v: %v", subject, pending, err)
			}
		}
	}
	check(true)
	if err = h.Gateway.Resources.CheckPendingCostOwner(t.Context(), uuid.NewString()); err != nil {
		t.Fatalf("unrelated budget quarantined: %v", err)
	}
	// Replay the actual forward migration over already-owned templates in this
	// uniquely created scratch database, rather than hand-building its projection.
	if _, err = h.Pool.Exec(t.Context(), `DROP TRIGGER project_pending_resource_usage ON olp.provider_resources; DROP FUNCTION olp.project_pending_resource_usage(); DROP FUNCTION olp.has_pending_resource_usage(uuid); DROP FUNCTION olp.pending_resource_subjects(olp.provider_resources); DROP TRIGGER pending_recorded_owners ON olp.provider_resource_pending_usage; DROP FUNCTION olp.project_pending_recorded_owners(); DROP TABLE olp.pending_resource_recorded_owners; DROP TABLE olp.provider_resource_pending_usage; DROP INDEX olp.provider_resources_expiry; DROP FUNCTION olp.resource_owns_pending_usage(text)`); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../internal/database/migrations/0060_pending_resource_accounting.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Pool.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	check(true)
	event.Attempts[0].Routing.BudgetExempt = true
	raw, _ = json.Marshal(event)
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=jsonb_set(metadata,'{pending_usage}',$2::jsonb) WHERE id=$1`, id, raw); err != nil {
		t.Fatal(err)
	}
	check(false)
	// Removal of the sole template authority removes every projected subject.
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET metadata=metadata-'pending_usage' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	check(false)
}
