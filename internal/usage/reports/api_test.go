package reports

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/param"
)

func usageRequest(t *testing.T, query string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, "/api/v1/usage/summary?"+query, nil)
}

func TestUsageQueryRequiresABoundedRange(t *testing.T) {
	filters, err := usageFilters(usageRequest(t,
		"start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00%2B02:00&route=+primary+&model="), authz.Principal{})
	if err != nil {
		t.Fatalf("usage filters: %v", err)
	}
	if filters.Start.Location() != time.UTC || filters.End.Location() != time.UTC {
		t.Fatalf("range %v..%v is not pinned to UTC", filters.Start, filters.End)
	}
	if filters.End.Format(time.RFC3339) != "2026-01-01T22:00:00Z" {
		t.Fatalf("end = %s, want the offset converted to UTC", filters.End.Format(time.RFC3339))
	}
	if filters.Route == nil || *filters.Route != "primary" {
		t.Fatalf("route = %v, want the trimmed value", filters.Route)
	}
	if filters.Model != nil {
		t.Fatalf("model = %v, want a cleared filter to be absent", *filters.Model)
	}
	for _, query := range []string{
		"end=2026-01-02T00:00:00Z",
		"start=2026-01-01T00:00:00Z",
		"start=yesterday&end=2026-01-02T00:00:00Z",
		"start=2026-01-01&end=2026-01-02T00:00:00Z",
	} {
		_, err = usageFilters(usageRequest(t, query), authz.Principal{})
		if problem := usageProblem(t, err); problem.Status != 400 || problem.Code != "invalid_range" {
			t.Fatalf("query %q gave %d %s, want 400 invalid_range", query, problem.Status, problem.Code)
		}
	}
	_, err = usageFilters(usageRequest(t,
		"start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z&provider_id=nope"), authz.Principal{})
	if problem := usageProblem(t, err); problem.Status != 400 || problem.Code != "invalid_filter" {
		t.Fatalf("problem = %d %s, want 400 invalid_filter", problem.Status, problem.Code)
	}
}

func TestUsagePagingParametersAreBounded(t *testing.T) {
	empty := url.Values{}
	limit, err := param.Limit(empty)
	if err != nil || limit != 50 {
		t.Fatalf("default limit = %d, %v; want 50", limit, err)
	}
	for _, valid := range []string{"1", "200"} {
		if _, err = param.Limit(url.Values{"limit": {valid}}); err != nil {
			t.Fatalf("limit %q was rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{"0", "201", "-1", "many", "1.5"} {
		_, err = param.Limit(url.Values{"limit": {invalid}})
		if problem := usageProblem(t, err); problem.Status != 400 || problem.Code != "invalid_limit" {
			t.Fatalf("limit %q gave %d %s, want 400 invalid_limit", invalid, problem.Status, problem.Code)
		}
	}
	cursor, err := param.Cursor(empty)
	if err != nil || cursor != nil {
		t.Fatalf("absent cursor = %v, %v", cursor, err)
	}
	at := time.Date(2026, 2, 3, 4, 5, 6, 7, time.UTC)
	id := "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"
	cursor, err = param.Cursor(url.Values{"cursor": {usage.EncodeCursor(at, id)}})
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if !cursor.At.Equal(at) || cursor.ID != id {
		t.Fatalf("cursor = %+v, want %v/%s", cursor, at, id)
	}
	_, err = param.Cursor(url.Values{"cursor": {"not-a-cursor"}})
	if problem := usageProblem(t, err); problem.Status != 400 || problem.Code != "invalid_cursor" {
		t.Fatalf("problem = %d %s, want 400 invalid_cursor", problem.Status, problem.Code)
	}
}

func TestUsageComparisonRequiresAProjectAndClosedUTCDays(t *testing.T) {
	const project = "0d2f5f4e-2b8a-4a51-8f3c-5c7a3e9b1d20"
	closedRange := "start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z"
	api := &API{}
	for _, test := range []struct {
		note, query, code string
		principal         authz.Principal
		status            int
	}{
		{"missing project", closedRange, "invalid_filter", authz.Principal{AllProjects: true}, 400},
		{"repeated project", closedRange + "&project_id=" + project + "&project_id=" + project, "invalid_filter", authz.Principal{AllProjects: true}, 400},
		{"inaccessible project", closedRange + "&project_id=" + project, "not_found", authz.Principal{}, 404},
	} {
		t.Run(test.note, func(t *testing.T) {
			_, err := api.usageComparison(usageRequest(t, test.query), test.principal)
			if got := usageProblem(t, reportProblem(err)); got.Status != test.status || got.Code != test.code {
				t.Fatalf("HTTP %d %s, want %d %s", got.Status, got.Code, test.status, test.code)
			}
		})
	}
	end := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	now := end.Add(12 * time.Hour)
	for _, days := range []int{1, 31} {
		start := end.AddDate(0, 0, -days)
		previous, err := comparisonBaseline(Filters{Start: start, End: end}, now)
		if err != nil || !previous.Start.Equal(start.AddDate(0, 0, -days)) || !previous.End.Equal(start) {
			t.Errorf("%d closed days gave previous range %s to %s (%v)", days, previous.Start, previous.End, err)
		}
	}
	for _, rangeFilter := range []Filters{
		{Start: end, End: end.AddDate(0, 0, 1)},
		{Start: end.Add(-time.Hour), End: end},
		{Start: end.AddDate(0, 0, -32), End: end},
		{Start: end.AddDate(0, 0, -2).Add(time.Nanosecond), End: end},
	} {
		if _, err := comparisonBaseline(rangeFilter, now); err == nil {
			t.Errorf("accepted non-comparable range %s to %s", rangeFilter.Start, rangeFilter.End)
		}
	}
}

func TestRequestFiltersApplyAttribution(t *testing.T) {
	key, value, bad := "team", "billing", "not a key!"
	if problem := usageProblem(t, RequestFilters{AttributionValue: &value}.validate()); problem.Status != 400 || problem.Code != "invalid_filter" {
		t.Fatalf("value without key gave %d %s, want 400 invalid_filter", problem.Status, problem.Code)
	}
	if problem := usageProblem(t, RequestFilters{AttributionKey: &bad}.validate()); problem.Status != 400 || problem.Code != "invalid_filter" {
		t.Fatalf("invalid key gave %d %s, want 400 invalid_filter", problem.Status, problem.Code)
	}
	filters := RequestFilters{AttributionKey: &key, AttributionValue: &value, Projects: AllProjects()}
	if err := filters.validate(); err != nil {
		t.Fatalf("a valid attribution filter was rejected: %v", err)
	}
	var q filterQuery
	filters.push(&q)
	if got, want := q.sql(), " AND r.attribution ? $1 AND r.attribution->>$2 = $3"; got != want {
		t.Fatalf("sql = %q, want %q", got, want)
	}
	if len(q.args) != 3 || q.args[0] != key || q.args[1] != key || q.args[2] != value {
		t.Fatalf("args = %v", q.args)
	}
}
