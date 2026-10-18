package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

type inspectionKeyQuery struct {
	access.Queryer
	values []any
}

func (q inspectionKeyQuery) QueryRow(context.Context, string, ...any) pgx.Row {
	return inspectionKeyRow(q.values)
}

type inspectionKeyRow []any

func (r inspectionKeyRow) Scan(dest ...any) error {
	if len(dest) != len(r) {
		return fmt.Errorf("got %d destinations for %d columns", len(dest), len(r))
	}
	for i, value := range r {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, dest[i]); err != nil {
			return err
		}
	}
	return nil
}

func TestInspectionKeyLoadsInheritedDecisionBudgets(t *testing.T) {
	for _, scope := range []string{"group", "group_template", "key_template", "project", "organization", "installation"} {
		t.Run(scope, func(t *testing.T) {
			keyID, projectID := uuid.NewString(), uuid.NewString()
			policy := access.KeyPolicy{Scopes: []string{"inference"}}
			values := make([]any, 22)
			values[3] = projectID
			values[4] = access.RouteGroups{}
			values[5] = access.LimitTemplates{"cap": {TokensPerMinute: new(int64(1000))}}
			values[14] = "cost"
			switch scope {
			case "group":
				values[6] = uuid.NewString()
				values[9] = int64(1000)
			case "group_template":
				values[6] = uuid.NewString()
				values[7] = "cap"
			case "key_template":
				policy.LimitTemplate = new("cap")
			case "project":
				values[17] = &access.BudgetPolicy{DailyCostLimit: new("1")}
			case "organization":
				values[19] = uuid.NewString()
				values[20] = &access.BudgetPolicy{DailyCostLimit: new("1")}
			case "installation":
				values[21] = &access.BudgetPolicy{DailyCostLimit: new("1")}
			}
			values[0], _ = json.Marshal(policy)
			key, err := (&Server{}).inspectionKey(httptest.NewRequest("POST", "/", nil), inspectionKeyQuery{values: values}, access.Principal{AllProjects: true}, &keyID, routing.Route{Slug: "route", ProjectID: &projectID})
			if err != nil {
				t.Fatal(err)
			}
			if !key.authority.UnboundedWorkBudgeted("route") {
				t.Fatalf("%s budget was not loaded", scope)
			}
		})
	}
}

func TestKeylessInspectionLoadsHierarchyBudgets(t *testing.T) {
	for _, scope := range []string{"project", "organization", "installation", "attribution"} {
		t.Run(scope, func(t *testing.T) {
			project := uuid.NewString()
			values := make([]any, 6)
			budget := &access.BudgetPolicy{DailyCostLimit: new("1")}
			switch scope {
			case "project":
				values[0] = budget
			case "organization":
				values[1] = uuid.NewString()
				values[2] = budget
			case "installation":
				values[5] = budget
			case "attribution":
				values[3] = access.AttributionBudgets{"team": {"core": *budget}}
				values[4] = map[string]any{"attribution_defaults": map[string]string{"team": "core"}}
			}
			key, err := (&Server{}).inspectionKey(httptest.NewRequest("POST", "/", nil), inspectionKeyQuery{values: values}, access.Principal{AllProjects: true}, nil, routing.Route{Slug: "route", ProjectID: &project})
			if err != nil {
				t.Fatal(err)
			}
			key.authority.Attribution, err = key.authority.ResolveAttribution(nil)
			if err != nil || !key.authority.UnboundedWorkBudgeted("route") {
				t.Fatalf("keyless %s budget omitted: %v", scope, err)
			}
		})
	}
}
