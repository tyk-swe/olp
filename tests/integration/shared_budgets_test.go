//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

// sbGroup creates a budget group with a daily budget, opens its balance, and
// returns its identifier.
func sbGroup(t *testing.T, f *glFixture, name, daily string) string {
	t.Helper()
	group := f.h.want(f.owner, "POST", "/api/v1/budget-groups",
		map[string]any{"name": name, "daily_cost_limit": daily}, idem("group-"+name), 201)
	id := group["id"].(string)
	kbOpen(t, f, id)
	return id
}

// The group's budget is used up, which is what the answer says.

func TestSharedBudgetAccounting(t *testing.T) {
	fixture := acctSeed(t, acctPool(t))
	newGroup := func(limit string) string {
		t.Helper()
		id := acctID(t)
		acctExec(t, fixture.Pool, `INSERT INTO olp.budget_groups
            (id, name, project_id, daily_cost_limit, etag, created_by)
            VALUES ($1::uuid, $2, NULL, $3::text::numeric, $4::uuid, $5::uuid)`,
			id, "group-"+id, limit, acctID(t), fixture.User)
		return id
	}
	groupA, groupB := newGroup("5.00"), newGroup("9.00")
	acctExec(t, fixture.Pool, `UPDATE olp.api_keys SET budget_group_id = $1::uuid WHERE id = $2::uuid`,
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
	if len(result.CostSnapshots) != 4 {
		t.Fatalf("cost snapshots = %d, want a cost and charge pair per owner", len(result.CostSnapshots))
	}
	// Each snapshot names the request it accounts for, which is how installing
	// it removes the cost reserved for that request from the key and the group.
	for _, snapshot := range result.CostSnapshots {
		if snapshot.RequestID != event.RequestID {
			t.Fatalf("snapshot for %s names request %q, want %q", snapshot.CostOwnerID, snapshot.RequestID, event.RequestID)
		}
	}
	var keySnapshot, groupSnapshot *limits.CostSnapshot
	for i := range result.CostSnapshots {
		if result.CostSnapshots[i].Basis != limits.BasisCost {
			continue
		}
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
        FROM olp.attempt_usage_facts WHERE request_id = $1::uuid`, event.RequestID).Scan(&storedGroup); err != nil {
		t.Fatalf("fact group: %v", err)
	}
	if storedGroup == nil || *storedGroup != groupA {
		t.Fatalf("fact budget_group_id = %v, want %s", storedGroup, groupA)
	}
	var requestGroup *string
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT budget_group_id::text
        FROM olp.requests WHERE id = $1::uuid`, event.RequestID).Scan(&requestGroup); err != nil {
		t.Fatalf("request group: %v", err)
	}
	if requestGroup == nil || *requestGroup != groupA {
		t.Fatalf("request budget_group_id = %v, want %s", requestGroup, groupA)
	}

	bucket := time.Now().UTC().Truncate(time.Hour)
	acctExec(t, fixture.Pool, `INSERT INTO olp.attempt_usage_hourly
        (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
         budget_group_id, request_count, provider_request_count, model_request_count,
         target_request_count, input_tokens, output_tokens, cached_input_tokens, media_units,
         estimated_cost, request_unpriced_count, provider_unpriced_count, model_unpriced_count,
         target_unpriced_count, request_incomplete_count, provider_incomplete_count,
         model_incomplete_count, target_incomplete_count, currency, unpriced_attempt_count)
        VALUES ($1, 'chat', $2::uuid, 'gpt-4o-mini', 'generation', 'openai', $3::uuid, $4::uuid,
         1, 1, 1, 1, 10, 10, 0, 0, '1.25'::numeric, 0, 0, 0, 0, 0, 0, 0, 0, 'USD', 0)`,
		bucket, fixture.Provider, fixture.Key, groupA)

	acctExec(t, fixture.Pool, `UPDATE olp.api_keys SET budget_group_id = $1::uuid WHERE id = $2::uuid`,
		groupB, fixture.Key)
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT budget_group_id::text
        FROM olp.attempt_usage_facts WHERE request_id = $1::uuid`, event.RequestID).Scan(&storedGroup); err != nil {
		t.Fatalf("fact group after move: %v", err)
	}
	if storedGroup == nil || *storedGroup != groupA {
		t.Fatalf("reattributed persisted fact: %v", storedGroup)
	}
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp.attempt_usage_hourly
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
		if snapshots[i].CostOwnerID == groupA && snapshots[i].Basis != limits.BasisCharge {
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
	if result := acctPersist(t, fixture, second); len(result.CostSnapshots) != 4 {
		t.Fatalf("second event snapshots = %d, want 4", len(result.CostSnapshots))
	}
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT budget_group_id::text
        FROM olp.attempt_usage_facts WHERE request_id = $1::uuid`, second.RequestID).Scan(&storedGroup); err != nil {
		t.Fatalf("second fact group: %v", err)
	}
	if storedGroup == nil || *storedGroup != groupA {
		t.Fatalf("second fact attributed to %v, want the admitted group %s", storedGroup, groupA)
	}
}

// No reply bound, so the request could cost far more than the group holds.

// Nothing is spent, so the group is not exhausted: it is the request that does
// not fit.

// A member's request in flight holds the group's budget against the others.

// It cost 0.0064, which is what the group now holds, and the member has no
// budget of its own to hold anything.

// Only what the first request cost: the second held the group's budget
// for the moment it took to be refused.

// The member needs a hard limit of its own: without one admission hands back
// the group's lease as it is, and the lease that finishes both is never made.
