package usage

import (
	"reflect"
	"strings"
	"testing"
)

func TestRequestProjectAndSessionFiltersDoNotReplaceCallerScope(t *testing.T) {
	allowed := []string{"01990000-0000-7000-8000-000000000004"}
	project, key, session := "01990000-0000-7000-8000-000000000009", "session", "private-session"
	filters := RequestFilters{
		AllowedProjects: allowed, ProjectID: &project, AttributionKey: &key, AttributionValue: &session,
	}
	if err := filters.Validate(); err != nil {
		t.Fatal(err)
	}
	var query filterQuery
	filters.push(&query)
	if !strings.Contains(query.sql(), "r.api_key_id IN (SELECT id FROM olp.api_keys WHERE project_id = ANY($1::uuid[]))") ||
		!strings.Contains(query.sql(), requestProjectExpression+" = $2::uuid") ||
		!strings.Contains(query.sql(), "r.attribution->>$4 = $5") {
		t.Fatalf("scope or session missing from query: %s", query.sql())
	}
	if !reflect.DeepEqual(query.args, []any{allowed, project, key, key, session}) {
		t.Fatalf("filters were interpolated or rebound: %v", query.args)
	}
	filters.AllProjects = true
	query = filterQuery{}
	filters.push(&query)
	if strings.Contains(query.sql(), "r.api_key_id IN") || !strings.Contains(query.sql(), requestProjectExpression+" = $1::uuid") {
		t.Fatalf("installation project selection: %s", query.sql())
	}
}
