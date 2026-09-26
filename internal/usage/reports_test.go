package usage

import (
	"context"
	"strings"
	"testing"
	"time"
)

func usageFilterRange(t *testing.T, start, end string) Filters {
	t.Helper()
	from, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		t.Fatalf("parse %q: %v", start, err)
	}
	to, err := time.Parse(time.RFC3339Nano, end)
	if err != nil {
		t.Fatalf("parse %q: %v", end, err)
	}
	return Filters{Start: from.UTC(), End: to.UTC(), AllProjects: true}
}

func TestUsageHourRoundingIsExactOnBothSidesOfTheEpoch(t *testing.T) {
	for _, c := range []struct{ value, floor, ceil string }{
		{"2026-07-12T10:00:00Z", "2026-07-12T10:00:00Z", "2026-07-12T10:00:00Z"},
		{"2026-07-12T10:59:59.999Z", "2026-07-12T10:00:00Z", "2026-07-12T11:00:00Z"},
		{"1969-12-31T23:59:59Z", "1969-12-31T23:00:00Z", "1970-01-01T00:00:00Z"},
		{"1970-01-01T00:00:00Z", "1970-01-01T00:00:00Z", "1970-01-01T00:00:00Z"},
	} {
		value, err := time.Parse(time.RFC3339Nano, c.value)
		if err != nil {
			t.Fatalf("parse %q: %v", c.value, err)
		}
		if got := floorHour(value).Format(time.RFC3339Nano); got != c.floor {
			t.Fatalf("floorHour(%s) = %s, want %s", c.value, got, c.floor)
		}
		if got := ceilHour(value).Format(time.RFC3339Nano); got != c.ceil {
			t.Fatalf("ceilHour(%s) = %s, want %s", c.value, got, c.ceil)
		}
	}
}

func TestUsageCountScopeFollowsTheFilteredDimension(t *testing.T) {
	provider, model := "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000", "gpt-test"
	for _, c := range []struct {
		provider, model bool
		count, hourly   string
	}{
		{false, false, "request_counted", "request_count"},
		{true, false, "provider_request_counted", "provider_request_count"},
		{false, true, "model_request_counted", "model_request_count"},
		{true, true, "target_request_counted", "target_request_count"},
	} {
		filters := usageFilterRange(t, "2026-01-01T00:00:00Z", "2026-01-01T01:00:00Z")
		if c.provider {
			filters.ProviderID = &provider
		}
		if c.model {
			filters.Model = &model
		}
		scope := scopeFor(filters)
		if scope.count != c.count || scope.hourlyCount != c.hourly {
			t.Fatalf("scope for provider=%v model=%v = %s/%s, want %s/%s",
				c.provider, c.model, scope.count, scope.hourlyCount, c.count, c.hourly)
		}
	}
}

func TestUsageRangeIsPositiveAndBoundedToOneYear(t *testing.T) {
	start := "2026-01-01T00:00:00Z"
	valid := []string{"2026-01-01T00:00:00.000000001Z", "2027-01-02T00:00:00Z"}
	for _, end := range valid {
		if err := usageFilterRange(t, start, end).Validate(); err != nil {
			t.Fatalf("range %s..%s was rejected: %v", start, end, err)
		}
	}
	invalid := []string{start, "2025-12-31T23:59:59.999999999Z", "2027-01-02T00:00:00.000000001Z"}
	for _, end := range invalid {
		err := usageFilterRange(t, start, end).Validate()
		if err == nil {
			t.Fatalf("range %s..%s was accepted", start, end)
		}
		if problem := usageProblem(t, err); problem.Status != 400 || problem.Code != "invalid_range" {
			t.Fatalf("problem = %d %s, want 400 invalid_range", problem.Status, problem.Code)
		}
	}
}

func TestUsageOperationFilterMustNameARealOperation(t *testing.T) {
	filters := usageFilterRange(t, "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z")
	generation := "generation"
	filters.Operation = &generation
	if err := filters.Validate(); err != nil {
		t.Fatalf("a known operation was rejected: %v", err)
	}
	telepathy := "telepathy"
	filters.Operation = &telepathy
	problem := usageProblem(t, filters.Validate())
	if problem.Status != 400 || problem.Code != "invalid_operation" {
		t.Fatalf("problem = %d %s, want 400 invalid_operation", problem.Status, problem.Code)
	}
}

func TestUsageRowsSpanLiveFactsAndRetainedBuckets(t *testing.T) {
	filters := usageFilterRange(t, "2026-01-01T00:15:00Z", "2026-01-01T03:45:00Z")
	route, provider := "primary", "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
	model, key, operation := "gpt-test", "0199bbbb-cccc-7ddd-8eee-ffff00001111", "generation"
	filters.Route, filters.ProviderID, filters.Model = &route, &provider, &model
	filters.APIKey, filters.Operation = &key, &operation
	var query filterQuery
	filters.usageRows(&query, scopeFor(filters))
	sql := query.sql()
	for _, fragment := range []string{
		"FROM olp.attempt_usage_facts", "target_request_counted", "target_unpriced_counted",
		"target_incomplete_counted", "FROM olp.attempt_usage_hourly", "target_request_count",
		"target_unpriced_count", "target_incomplete_count", "observed_at >= $", "observed_at < $",
		"bucket >= $", "bucket + interval '1 hour' <= $", "route_slug = $", "provider_id = $",
		"upstream_model = $", "api_key_id = $", "operation = $", "UNION ALL",
	} {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("usage rows CTE is missing %q:\n%s", fragment, sql)
		}
	}
	// Both halves bind the same five dimensions plus their own bounds.
	if len(query.args) != 14 {
		t.Fatalf("bound %d arguments, want 14: %v", len(query.args), query.args)
	}
	if !strings.Contains(sql, "$"+strings.TrimSpace("14")) {
		t.Fatalf("the last placeholder is unused:\n%s", sql)
	}
	// The retained half starts at the first whole bucket inside the range.
	if at, ok := query.args[7].(time.Time); !ok || !at.Equal(ceilHour(filters.Start)) {
		t.Fatalf("retained lower bound = %v, want %v", query.args[7], ceilHour(filters.Start))
	}
}

func TestUsageReportsRejectUnknownGroupings(t *testing.T) {
	ctx := context.Background()
	filters := usageFilterRange(t, "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z")
	q := consumerQueryer{}
	_, err := ReadBreakdown(ctx, q, filters, "sideways", 10)
	if problem := usageProblem(t, err); problem.Status != 400 || problem.Code != "invalid_dimension" {
		t.Fatalf("problem = %d %s, want 400 invalid_dimension", problem.Status, problem.Code)
	}
	_, err = ReadSeries(ctx, q, filters, "fortnight")
	if problem := usageProblem(t, err); problem.Status != 400 || problem.Code != "invalid_granularity" {
		t.Fatalf("problem = %d %s, want 400 invalid_granularity", problem.Status, problem.Code)
	}
	// A known dimension is not refused before the database is consulted.
	if _, err = ReadBreakdown(ctx, q, filters, DimensionAPIKey, 10); err == nil {
		t.Fatal("a known dimension did not reach the database")
	} else if strings.Contains(err.Error(), "invalid_dimension") {
		t.Fatalf("a known dimension was refused: %v", err)
	}
}

func TestUsageTotalsRejectNegativeStoredCounts(t *testing.T) {
	totals := Totals{RequestCount: 1, UnpricedCount: 0, IncompleteCount: 0}
	if !totals.valid() {
		t.Fatal("a non-negative total was rejected")
	}
	for _, invalid := range []Totals{{RequestCount: -1}, {UnpricedCount: -1}, {IncompleteCount: -1}} {
		if invalid.valid() {
			t.Fatalf("negative totals %+v were accepted", invalid)
		}
	}
}
