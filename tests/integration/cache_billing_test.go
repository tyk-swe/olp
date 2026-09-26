//go:build integration

package integration_test

import (
	"errors"
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

func TestCacheBillingExactPrices(t *testing.T) {
	fixture := acctSeed(t, acctPool(t))
	observed := time.Now().UTC().Add(-time.Minute)
	attempt := func() usage.Attempt {
		return acctAttempt(t, fixture.Provider, 1, "gpt-4o-mini", 200,
			cbUsage(100, 10, 20, 30, 10, 5))
	}

	acctPricing(t, fixture, 1, observed.Add(-time.Hour),
		acctPrice{Kind: "openai", ProviderID: acctPtr(fixture.Provider), Model: "gpt-4o-mini",
			Operation: "generation", Input: acctPtr("1"), Cached: acctPtr("0.1"),
			Write: acctPtr("1.25"), Write5M: acctPtr("1.2"), Write1H: acctPtr("2"),
			Output: acctPtr("2")})
	event := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed,
		Attempts: []usage.Attempt{attempt()}})
	acctPersist(t, fixture, event)
	fact := acctLoadFact(t, fixture, event.RequestID, 1)
	if fact.Cost == nil || *fact.Cost != "0.000112750000" {
		t.Fatalf("exact cache cost = %v, want 0.000112750000", fact.Cost)
	}

	var cached, write, write5m, write1h *int64
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT cached_input_tokens,
        cache_write_input_tokens, cache_write_5m_input_tokens, cache_write_1h_input_tokens
        FROM olp.attempt_usage_facts WHERE request_id = $1::uuid`,
		event.RequestID).Scan(&cached, &write, &write5m, &write1h); err != nil {
		t.Fatalf("load cache columns: %v", err)
	}
	if *cached != 20 || *write != 30 || *write5m != 10 || *write1h != 5 {
		t.Fatalf("stored cache categories = %v/%v/%v/%v", cached, write, write5m, write1h)
	}

	acctPricing(t, fixture, 2, observed.Add(-time.Hour),
		acctPrice{Kind: "openai", ProviderID: acctPtr(fixture.Provider), Model: "gpt-4o-mini",
			Operation: "generation", Input: acctPtr("1"), Output: acctPtr("2")})
	fallback := acctEvent(t, fixture, acctEventOptions{ObservedAt: observed.Add(time.Second),
		Attempts: []usage.Attempt{attempt()}})
	acctPersist(t, fixture, fallback)
	fact = acctLoadFact(t, fixture, fallback.RequestID, 1)
	if fact.Cost == nil || *fact.Cost != "0.000120000000" {
		t.Fatalf("fallback cost = %v, want 0.000120000000", fact.Cost)
	}
}

func TestCacheBillingValidation(t *testing.T) {
	fixture := acctSeed(t, acctPool(t))
	for _, tc := range []struct {
		name  string
		usage *usage.AttemptUsage
	}{
		{"read plus write beyond input", cbUsage(40, 10, 20, 30, 10, 5)},
		{"detail beyond generic write", cbUsage(100, 10, 20, 30, 20, 20)},
		{"negative write", cbUsage(100, 10, 20, -1, 0, 0)},
		{"negative detail", cbUsage(100, 10, 20, 30, -1, 0)},
		{"write without input", &usage.AttemptUsage{Observed: true, Complete: true,
			OutputTokens: acctPtr(int64(10)), CacheWriteInputTokens: acctPtr(int64(30))}},
		{"cache on unobserved usage", &usage.AttemptUsage{Complete: true,
			CacheWriteInputTokens: acctPtr(int64(30))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := acctEvent(t, fixture, acctEventOptions{Attempts: []usage.Attempt{
				acctAttempt(t, fixture.Provider, 1, "gpt-4o-mini", 200, tc.usage),
			}})
			if _, err := acctPersistErr(t, fixture, event); !errors.Is(err, usage.ErrInvalidEvent) {
				t.Fatalf("accepted %s: %v", tc.name, err)
			}
		})
	}

	insert := func(input, cached, write, write5m, write1h any) error {
		request := acctID(t)
		started := time.Now().UTC()
		acctExec(t, fixture.Pool, `INSERT INTO olp.usage_request_anchors
            (request_id, request_started_at) VALUES ($1::uuid, $2)`, request, started)
		_, err := fixture.Pool.Exec(t.Context(), `INSERT INTO olp.attempt_usage_facts
            (attempt_id, event_id, request_id, request_started_at, attempt_ordinal, api_key_id,
             provider_id, route_slug, upstream_model, operation, surface, observed_at,
             charge_status, usage_observed, usage_complete, input_tokens, cached_input_tokens,
             cache_write_input_tokens, cache_write_5m_input_tokens, cache_write_1h_input_tokens,
             unpriced, request_counted, provider_request_counted, model_request_counted,
             target_request_counted, request_unpriced_counted, provider_unpriced_counted,
             model_unpriced_counted, target_unpriced_counted, request_incomplete_counted,
             provider_incomplete_counted, model_incomplete_counted, target_incomplete_counted)
            VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 1, $5::uuid, $6::uuid, 'chat', 'm',
             'generation', 'openai', $4, 'billable', true, true, $7, $8, $9, $10, $11,
             false, true, true, true, true, false, false, false, false, false, false, false, false)`,
			acctID(t), acctID(t), request, started, fixture.Key, fixture.Provider,
			input, cached, write, write5m, write1h)
		return err
	}
	if err := insert(40, 20, 30, 10, 5); err == nil {
		t.Fatal("constraint accepted read plus write beyond input")
	}
	if err := insert(100, 20, 30, 20, 20); err == nil {
		t.Fatal("constraint accepted detail beyond generic write")
	}
	if err := insert(nil, 20, nil, nil, nil); err == nil {
		t.Fatal("constraint accepted a cache read without input tokens")
	}
	if err := insert(100, 20, nil, 5, nil); err == nil {
		t.Fatal("constraint accepted cache detail without a generic write total")
	}
	if err := insert(100, 20, 30, 10, 5); err != nil {
		t.Fatalf("constraint rejected valid categories: %v", err)
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
