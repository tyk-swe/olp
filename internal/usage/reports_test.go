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

// Both halves bind the same five dimensions plus their own bounds.

// The retained half starts at the first whole bucket inside the range.

// Requests count once per request here, so the rows sum to the range's
// requests however many families a failover crossed.

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
