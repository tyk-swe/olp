package reports

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
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
	return Filters{Start: from.UTC(), End: to.UTC(), Projects: AllProjects()}
}

func TestUsageReportsRejectUnknownGroupings(t *testing.T) {
	filters := usageFilterRange(t, "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z")
	key := "team"
	_, unknownDimension := parseDimension("sideways", filters)
	_, unkeyedAttribution := parseDimension(string(DimensionAttribution), filters)
	_, unknownGranularity := parseGranularity("fortnight")
	cases := []struct {
		name, code, detail string
		err                error
	}{
		{"unknown dimension", "invalid_dimension", "Dimension must be", unknownDimension},
		{"attribution without its key", "invalid_filter", "The attribution breakdown requires the attribution_key filter.", unkeyedAttribution},
		{"unknown granularity", "invalid_granularity", "Granularity must be hour or day.", unknownGranularity},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := usageProblem(t, test.err)
			if got.Status != 400 || got.Code != test.code || !strings.HasPrefix(got.Detail, test.detail) {
				t.Fatalf("problem = %d %s %q, want 400 %s %q", got.Status, got.Code, got.Detail, test.code, test.detail)
			}
		})
	}
	filters.AttributionKey = &key
	if dimension, err := parseDimension(string(DimensionAttribution), filters); err != nil || dimension != DimensionAttribution {
		t.Fatalf("attribution with its key = %q, %v", dimension, err)
	}
}

func TestOversizedExportIsUnprocessable(t *testing.T) {
	_, err := RequestCSV(make([]RequestSummary, 10001))
	got := usageProblem(t, err)
	if got.Status != 422 || got.Code != "export_too_large" {
		t.Fatalf("problem = %d %s, want 422 export_too_large", got.Status, got.Code)
	}
}

func TestInternalFailuresKeepTheirCause(t *testing.T) {
	cause := errors.New("database went away")
	if !errors.Is(problem(cause), cause) {
		t.Fatal("an internal failure was rewritten as a report failure")
	}
}

func usageProblem(t *testing.T, err error) *access.Problem {
	t.Helper()
	var mapped *access.Problem
	if !errors.As(problem(err), &mapped) {
		t.Fatalf("error %v is not a management problem", err)
	}
	return mapped
}
