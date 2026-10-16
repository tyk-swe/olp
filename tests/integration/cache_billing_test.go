//go:build integration

package integration_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

func cbUsage(input, output, cached, write, write5m, write1h int64) *usage.AttemptUsage {
	return &usage.AttemptUsage{
		Observed: true, Complete: true,
		InputTokens:             acctPtr(input),
		OutputTokens:            acctPtr(output),
		CachedInputTokens:       acctPtr(cached),
		CacheWriteInputTokens:   acctPtr(write),
		CacheWrite5MInputTokens: acctPtr(write5m),
		CacheWrite1HInputTokens: acctPtr(write1h),
	}
}

func TestCacheBillingRollupPreservesCategories(t *testing.T) {
	fixture := acctSeed(t, acctPool(t))
	group := acctID(t)
	acctExec(t, fixture.Pool, `INSERT INTO olp.budget_groups
        (id, name, project_id, daily_cost_limit, etag, created_by)
        VALUES ($1::uuid, $2, NULL, '5.00', $3::uuid, $4::uuid)`,
		group, "rollup-group", acctID(t), fixture.User)

	observed := time.Now().UTC().Add(-time.Minute)
	grouped := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed, Attempts: []usage.Attempt{
		acctAttempt(t, fixture.Provider, 1, "gpt-4o-mini", 200, cbUsage(100, 10, 20, 30, 10, 5)),
	}})
	grouped.BudgetGroupID = acctPtr(group)
	acctPersist(t, fixture, grouped)
	ungrouped := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed.Add(time.Second),
		Attempts: []usage.Attempt{
			acctAttempt(t, fixture.Provider, 1, "gpt-4o-mini", 200, cbUsage(50, 5, 10, 15, 5, 2)),
		}})
	acctPersist(t, fixture, ungrouped)

	if _, err := usage.RunMaintenance(t.Context(), fixture.Pool, observed.Add(91*24*time.Hour)); err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp.attempt_usage_facts`); count != 0 {
		t.Fatalf("unrolled facts remain: %d", count)
	}
	if count := acctCount(t, fixture, `SELECT count(*) FROM olp.attempt_usage_hourly
        WHERE api_key_id = $1::uuid`, fixture.Key); count != 2 {
		t.Fatalf("rollups = %d, want one row per budget-group dimension", count)
	}
	var storedGroup *string
	var write, write5m, write1h int64
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT budget_group_id::text,
        cache_write_input_tokens, cache_write_5m_input_tokens, cache_write_1h_input_tokens
        FROM olp.attempt_usage_hourly WHERE budget_group_id IS NOT NULL`).Scan(
		&storedGroup, &write, &write5m, &write1h); err != nil {
		t.Fatalf("grouped rollup: %v", err)
	}
	if *storedGroup != group || write != 30 || write5m != 10 || write1h != 5 {
		t.Fatalf("grouped rollup = %s %d/%d/%d", *storedGroup, write, write5m, write1h)
	}

	summary, err := usage.ReadSummary(t.Context(), fixture.Pool,
		usage.Filters{Start: observed.Add(-time.Hour), End: observed.Add(time.Hour), AllProjects: true},
		observed.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if summary.CacheWriteInputTokens != "45" || summary.CacheWrite5MInputTokens != "15" ||
		summary.CacheWrite1HInputTokens != "7" || summary.CachedInputTokens != "30" {
		t.Fatalf("report cache totals: %+v", summary.Totals)
	}
	var rolled struct {
		Total, Group string
	}
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT
        COALESCE(SUM(cache_write_input_tokens),0)::text,
        COALESCE(SUM(cache_write_input_tokens) FILTER (WHERE budget_group_id IS NOT NULL),0)::text
        FROM olp.attempt_usage_hourly`).Scan(&rolled.Total, &rolled.Group); err != nil {
		t.Fatalf("rollup totals: %v", err)
	}
	if rolled.Total != "45" || rolled.Group != "30" {
		t.Fatalf("rollup cache-write totals = %s/%s, want 45/30", rolled.Total, rolled.Group)
	}
}
