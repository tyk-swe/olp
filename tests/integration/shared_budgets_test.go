//go:build integration

package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestSharedBudgetAdmission(t *testing.T) {
	c := limClient(t)
	f := glSeed(t, limLimiter(t, c, limNamespace(t, c, "shared-budget")), limits.FailClosed)
	group := f.h.want(f.owner, "POST", "/api/v1/budget-groups",
		map[string]any{"name": "shared spend", "daily_cost_limit": "5.00"},
		idem("group-shared"), 201)
	groupID := group["id"].(string)

	_, secretA := f.key("group-a", map[string]any{"budget_group_id": groupID})
	_, secretB := f.key("group-b", map[string]any{"budget_group_id": groupID})

	served := f.vendor.chats.Load()
	if status, code, _ := f.chat(secretA); status != http.StatusServiceUnavailable || code != "distributed_limits_unavailable" {
		t.Fatalf("unreconciled group: %d %s", status, code)
	}
	if f.vendor.chats.Load() != served {
		t.Fatal("a request whose group budget could not be read reached the upstream")
	}

	windows := limits.BudgetWindows(time.Now().UTC())
	snapshot := limits.CostSnapshot{
		CostOwnerID: groupID, DailyWindowID: windows.DailyID, DailyAccrued: "0.00",
		MonthlyWindowID: windows.MonthlyID, MonthlyAccrued: "0.00",
	}
	if _, _, err := f.limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
		t.Fatalf("apply group balance: %v", err)
	}
	if status, code, _ := f.chat(secretA); status != http.StatusOK {
		t.Fatalf("first group member: %d %s", status, code)
	}
	if status, code, _ := f.chat(secretB); status != http.StatusOK {
		t.Fatalf("second group member: %d %s", status, code)
	}

	snapshot.DailyAccrued = "5.00"
	if _, _, err := f.limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
		t.Fatalf("apply exhausted group balance: %v", err)
	}
	served = f.vendor.chats.Load()
	if status, code, _ := f.chat(secretA); status != http.StatusTooManyRequests || code != "budget_exhausted" {
		t.Fatalf("first member over shared budget: %d %s", status, code)
	}
	if status, code, _ := f.chat(secretB); status != http.StatusTooManyRequests || code != "budget_exhausted" {
		t.Fatalf("second member must share the exhaustion: %d %s", status, code)
	}
	if f.vendor.chats.Load() != served {
		t.Fatal("a request over the shared budget reached the upstream")
	}

	_, solo := f.key("solo", map[string]any{"requests_per_minute": 10})
	if status, code, _ := f.chat(solo); status != http.StatusOK {
		t.Fatalf("independent key rejected by a group's budget: %d %s", status, code)
	}
}

func TestSharedBudgetAPI(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()

	group := h.want(owner, "POST", "/api/v1/budget-groups",
		map[string]any{"name": "ops budget", "daily_cost_limit": "10", "monthly_cost_limit": "100"},
		idem("group-ops"), 201)
	groupID := group["id"].(string)
	path := "/api/v1/budget-groups/" + groupID
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if detail["name"] != "ops budget" || detail["project_id"] != nil {
		t.Fatalf("group detail: %v", detail)
	}
	budget, ok := detail["budget"].(map[string]any)
	if !ok {
		t.Fatalf("group carries no live budget: %v", detail)
	}
	daily, ok := budget["daily"].(map[string]any)
	if !ok || daily["limit"] != "10.000000000000" || daily["accrued"] != "0" ||
		daily["remaining"] != "10.000000000000" {
		t.Fatalf("daily window: %v", budget["daily"])
	}
	if _, ok := daily["reset_at"].(string); !ok {
		t.Fatalf("daily reset_at: %v", daily)
	}
	if budget["unpriced_attempts"] != float64(0) || budget["enforcement_active"] != false {
		t.Fatalf("budget state: %v", budget)
	}
	monthly, ok := budget["monthly"].(map[string]any)
	if !ok || monthly["limit"] != "100.000000000000" {
		t.Fatalf("monthly window: %v", budget["monthly"])
	}

	updated := h.want(owner, "PATCH", path,
		map[string]any{"monthly_cost_limit": "250"}, etagHeader(detail), 200)
	if updated["etag"] == detail["etag"] {
		t.Fatal("update did not rotate the group ETag")
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	if detail["budget"].(map[string]any)["monthly"].(map[string]any)["limit"] != "250.000000000000" {
		t.Fatalf("monthly limit after patch: %v", detail["budget"])
	}

	if status, out, _ := h.request(owner, "PATCH", path,
		map[string]any{"project_id": nil}, etagHeader(detail)); status != 422 {
		t.Fatalf("project_id patch: %d %v", status, out)
	}

	list := h.want(owner, "GET", "/api/v1/budget-groups", nil, nil, 200)
	items, ok := list["items"].([]any)
	if !ok || len(items) != 1 || items[0].(map[string]any)["id"] != groupID {
		t.Fatalf("group list: %v", list)
	}

	if status, out, _ := h.request(owner, "POST", "/api/v1/budget-groups",
		map[string]any{"name": "OPS BUDGET", "daily_cost_limit": "1"},
		idem("group-dup")); status != 409 {
		t.Fatalf("duplicate group name: %d %v", status, out)
	}
	if status, _, _ := h.request(owner, "POST", "/api/v1/budget-groups",
		map[string]any{"name": "no limits"}, idem("group-none")); status != 422 {
		t.Fatalf("group without a limit: %d", status)
	}
	if status, _, _ := h.request(owner, "POST", "/api/v1/budget-groups",
		map[string]any{"name": "bad amount", "daily_cost_limit": "0"},
		idem("group-zero")); status != 422 {
		t.Fatalf("zero limit: %d", status)
	}

	project := createProject(h, owner, "team")
	projectGroup := h.want(owner, "POST", "/api/v1/budget-groups",
		map[string]any{"name": "team budget", "project_id": project, "daily_cost_limit": "3"},
		idem("group-team"), 201)
	projectGroupID := projectGroup["id"].(string)

	newKey := func(body map[string]any, want int) map[string]any {
		base := map[string]any{"name": "k" + uuid.NewString()[:8], "scopes": []string{"inference"}, "allowed_routes": []string{}}
		for k, v := range body {
			base[k] = v
		}
		return h.want(owner, "POST", "/api/v1/api-keys", base, idem("key-"+uuid.NewString()), want)
	}
	keyA := newKey(map[string]any{"budget_group_id": projectGroupID, "project_id": project}, 201)
	newKey(map[string]any{"budget_group_id": projectGroupID, "project_id": project}, 201)

	if status, out, _ := h.request(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "mismatch", "scopes": []string{"inference"}, "allowed_routes": []string{},
			"project_id": project, "budget_group_id": groupID}, idem("key-mismatch")); status != 422 {
		t.Fatalf("project-mismatched group: %d %v", status, out)
	}
	if status, out, _ := h.request(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "cross", "scopes": []string{"inference"}, "allowed_routes": []string{},
			"budget_group_id": projectGroupID}, idem("key-cross")); status != 422 {
		t.Fatalf("project group on a global key: %d %v", status, out)
	}
	if status, _, _ := h.request(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "ghost", "scopes": []string{"inference"}, "allowed_routes": []string{},
			"budget_group_id": uuid.NewString()}, idem("key-ghost")); status != 404 {
		t.Fatalf("unknown group: %d", status)
	}

	keyID := keyA["id"].(string)
	keyDetail := h.want(owner, "GET", "/api/v1/api-keys/"+keyID, nil, nil, 200)
	if keyDetail["budget_group_id"] != projectGroupID {
		t.Fatalf("assigned group: %v", keyDetail["budget_group_id"])
	}
	if status, out, _ := h.request(owner, "PATCH", "/api/v1/api-keys/"+keyID,
		map[string]any{"budget_group_id": groupID}, etagHeader(keyDetail)); status != 422 {
		t.Fatalf("cross-project group assignment: %d %v", status, out)
	}
	h.want(owner, "PATCH", "/api/v1/api-keys/"+keyID,
		map[string]any{"budget_group_id": nil}, etagHeader(keyDetail), 200)
	keyDetail = h.want(owner, "GET", "/api/v1/api-keys/"+keyID, nil, nil, 200)
	if keyDetail["budget_group_id"] != nil {
		t.Fatalf("group not cleared: %v", keyDetail["budget_group_id"])
	}
}

func TestSharedBudgetAccounting(t *testing.T) {
	fixture := acctSeed(t, acctPool(t))
	newGroup := func(limit string) string {
		t.Helper()
		id := acctID(t)
		acctExec(t, fixture.Pool, `INSERT INTO olp_go.budget_groups
            (id, name, project_id, daily_cost_limit, etag, created_by)
            VALUES ($1::uuid, $2, NULL, $3::text::numeric, $4::uuid, $5::uuid)`,
			id, "group-"+id, limit, acctID(t), fixture.User)
		return id
	}
	groupA, groupB := newGroup("5.00"), newGroup("9.00")
	acctExec(t, fixture.Pool, `UPDATE olp_go.api_keys SET budget_group_id = $1::uuid WHERE id = $2::uuid`,
		groupA, fixture.Key)

	observed := time.Now().UTC().Add(-time.Minute)
	acctPricing(t, fixture, 1, observed.Add(-time.Hour),
		acctPrice{Kind: "openai", ProviderID: acctPtr(fixture.Provider), Model: "gpt-4o-mini",
			Operation: "generation", Input: acctPtr("1"), Output: acctPtr("2")})

	event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed, Attempts: []usage.Attempt{
		acctAttempt(t, fixture.Provider, 1, "gpt-4o-mini", 200, acctObserved(100, 10, nil, nil)),
	}})
	event.BudgetGroupID = acctPtr(groupA)
	result := acctPersist(t, fixture, event)
	if result.Outcome != usage.PersistOutcomePersisted {
		t.Fatalf("outcome = %v, want persisted", result.Outcome)
	}
	if len(result.CostSnapshots) != 2 {
		t.Fatalf("cost snapshots = %d, want one per owner", len(result.CostSnapshots))
	}
	var keySnapshot, groupSnapshot *limits.CostSnapshot
	for i := range result.CostSnapshots {
		switch result.CostSnapshots[i].CostOwnerID {
		case fixture.Key:
			keySnapshot = &result.CostSnapshots[i]
		case groupA:
			groupSnapshot = &result.CostSnapshots[i]
		}
	}
	if keySnapshot == nil || groupSnapshot == nil {
		t.Fatalf("snapshots do not cover key and group: %+v", result.CostSnapshots)
	}
	wantCost := "0.00012"
	acctSameMoney(t, fixture, &groupSnapshot.DailyAccrued, wantCost)
	acctSameMoney(t, fixture, &keySnapshot.DailyAccrued, wantCost)

	var storedGroup *string
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT budget_group_id::text
        FROM olp_go.attempt_usage_facts WHERE request_id = $1::uuid`, event.RequestID).Scan(&storedGroup); err != nil {
		t.Fatalf("fact group: %v", err)
	}
	if storedGroup == nil || *storedGroup != groupA {
		t.Fatalf("fact budget_group_id = %v, want %s", storedGroup, groupA)
	}
	var requestGroup *string
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT budget_group_id::text
        FROM olp_go.requests WHERE id = $1::uuid`, event.RequestID).Scan(&requestGroup); err != nil {
		t.Fatalf("request group: %v", err)
	}
	if requestGroup == nil || *requestGroup != groupA {
		t.Fatalf("request budget_group_id = %v, want %s", requestGroup, groupA)
	}

	bucket := time.Now().UTC().Truncate(time.Hour)
	acctExec(t, fixture.Pool, `INSERT INTO olp_go.attempt_usage_hourly
        (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
         budget_group_id, request_count, provider_request_count, model_request_count,
         target_request_count, input_tokens, output_tokens, cached_input_tokens, media_units,
         estimated_cost, request_unpriced_count, provider_unpriced_count, model_unpriced_count,
         target_unpriced_count, request_incomplete_count, provider_incomplete_count,
         model_incomplete_count, target_incomplete_count, currency, unpriced_attempt_count)
        VALUES ($1, 'chat', $2::uuid, 'gpt-4o-mini', 'generation', 'openai', $3::uuid, $4::uuid,
         1, 1, 1, 1, 10, 10, 0, 0, '1.25'::numeric, 0, 0, 0, 0, 0, 0, 0, 0, 'USD', 0)`,
		bucket, fixture.Provider, fixture.Key, groupA)

	acctExec(t, fixture.Pool, `UPDATE olp_go.api_keys SET budget_group_id = $1::uuid WHERE id = $2::uuid`,
		groupB, fixture.Key)
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT budget_group_id::text
        FROM olp_go.attempt_usage_facts WHERE request_id = $1::uuid`, event.RequestID).Scan(&storedGroup); err != nil {
		t.Fatalf("fact group after move: %v", err)
	}
	if storedGroup == nil || *storedGroup != groupA {
		t.Fatalf("reattributed persisted fact: %v", storedGroup)
	}
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp_go.attempt_usage_hourly
        WHERE budget_group_id = $1::uuid`, groupA); count != 1 {
		t.Fatalf("reattributed rollup: %d group rows", count)
	}

	conn, err := fixture.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	snapshots, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), time.Now().UTC())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var reconciled *limits.CostSnapshot
	for i := range snapshots {
		if snapshots[i].CostOwnerID == groupA {
			reconciled = &snapshots[i]
		}
	}
	if reconciled == nil {
		t.Fatalf("group %s missing from reconciliation", groupA)
	}
	acctSameMoney(t, fixture, &reconciled.DailyAccrued, "1.25012")
	acctSameMoney(t, fixture, &reconciled.MonthlyAccrued, "1.25012")

	second := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed.Add(time.Second), Attempts: []usage.Attempt{
		acctAttempt(t, fixture.Provider, 1, "gpt-4o-mini", 200, acctObserved(10, 10, nil, nil)),
	}})
	second.BudgetGroupID = acctPtr(groupA)
	if result := acctPersist(t, fixture, second); len(result.CostSnapshots) != 2 {
		t.Fatalf("second event snapshots = %d, want 2", len(result.CostSnapshots))
	}
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT budget_group_id::text
        FROM olp_go.attempt_usage_facts WHERE request_id = $1::uuid`, second.RequestID).Scan(&storedGroup); err != nil {
		t.Fatalf("second fact group: %v", err)
	}
	if storedGroup == nil || *storedGroup != groupA {
		t.Fatalf("second fact attributed to %v, want the admitted group %s", storedGroup, groupA)
	}
}
