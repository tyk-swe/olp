package projects

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

type Policies struct {
	EndUser            *authority.EndUserPolicy
	Attribution        *attribution.Policy
	RouteGroups        authority.RouteGroups
	Budget             *authority.BudgetPolicy
	Templates          authority.LimitTemplates
	AttributionBudgets authority.AttributionBudgets
}

const policyColumns = "end_user_policy,attribution_policy,route_groups,budget_policy,limit_templates,attribution_budgets"

func (p *Policies) targets() []any {
	return []any{&p.EndUser, &p.Attribution, &p.RouteGroups, &p.Budget, &p.Templates, &p.AttributionBudgets}
}

func ReadPolicies(ctx context.Context, tx pgx.Tx, id string) (Policies, string, error) {
	var current Policies
	var etag string
	err := tx.QueryRow(ctx, "SELECT "+policyColumns+",etag::text FROM olp.projects WHERE id=$1 FOR UPDATE", id).Scan(append(current.targets(), &etag)...)
	return current, etag, err
}

func WritePolicies(ctx context.Context, tx pgx.Tx, id string, desired Policies) (string, error) {
	endUser, err := json.Marshal(desired.EndUser)
	if err != nil {
		return "", err
	}
	labels, err := json.Marshal(desired.Attribution)
	if err != nil {
		return "", err
	}
	budget, err := json.Marshal(desired.Budget)
	if err != nil {
		return "", err
	}
	if desired.Budget.Limited() {
		if err := ledger.EnsureAggregateBudget(ctx, tx, "project", id); err != nil {
			return "", err
		}
	}
	if err := EnsureAttributionBudgetAccounts(ctx, tx, id, desired.AttributionBudgets); err != nil {
		return "", err
	}
	etag := access.NewID()
	_, writeErr := tx.Exec(ctx, `UPDATE olp.projects SET end_user_policy=NULLIF($2::jsonb,'null'::jsonb),etag=$3,updated_at=now(),attribution_policy=NULLIF($4::jsonb,'null'::jsonb),route_groups=$5::jsonb,budget_policy=NULLIF($6::jsonb,'null'::jsonb),limit_templates=$7::jsonb,attribution_budgets=$8::jsonb WHERE id=$1`, id, endUser, etag, labels, desired.RouteGroups.JSON(), budget, desired.Templates.JSON(), desired.AttributionBudgets.JSON())
	return etag, writeErr
}

type PolicyImport struct {
	ProjectID          string
	EndUserDefaults    *authority.AdmissionLimits
	EndUserTemplate    *string
	Attribution        *attribution.Policy
	RouteGroups        authority.RouteGroups
	Budget             *authority.BudgetPolicy
	Templates          *authority.LimitTemplates
	AttributionBudgets authority.AttributionBudgets
}

func (desired PolicyImport) policies(current Policies) Policies {
	templates := current.Templates
	if desired.Templates != nil {
		templates = *desired.Templates
	}
	return Policies{EndUser: ImportedEndUserPolicy(current.EndUser, desired.EndUserDefaults, desired.EndUserTemplate), Attribution: desired.Attribution, RouteGroups: desired.RouteGroups, Budget: desired.Budget, Templates: templates, AttributionBudgets: desired.AttributionBudgets}
}

func ImportedEndUserPolicy(existing *authority.EndUserPolicy, defaults *authority.AdmissionLimits, template *string) *authority.EndUserPolicy {
	if defaults == nil && template == nil && (existing == nil || len(existing.Overrides)+len(existing.Blocked) == 0) {
		return nil
	}
	values := authority.AdmissionLimits{}
	if defaults != nil {
		values = *defaults
	}
	if existing == nil {
		return &authority.EndUserPolicy{LimitTemplate: template, Defaults: values}
	}
	return &authority.EndUserPolicy{LimitTemplate: template, Defaults: values, Overrides: existing.Overrides, Blocked: existing.Blocked}
}

func (desired PolicyImport) Changes(current Policies) bool {
	return !current.equal(desired.policies(current))
}

func (current Policies) equal(desired Policies) bool {
	return current.AttributionBudgets.Equal(desired.AttributionBudgets) && reflect.DeepEqual(current.Templates, desired.Templates) && reflect.DeepEqual(current.EndUser, desired.EndUser) && reflect.DeepEqual(current.Attribution, desired.Attribution) && current.RouteGroups.Equal(desired.RouteGroups) && reflect.DeepEqual(current.Budget, desired.Budget)
}

func ApplyPolicies(ctx context.Context, tx pgx.Tx, principal access.Principal, changes []PolicyImport) (bool, error) {
	if len(changes) == 0 {
		return false, nil
	}
	current, _, err := ReadPolicies(ctx, tx, changes[0].ProjectID)
	if err != nil {
		return false, err
	}
	desired := changes[0].policies(current)
	changed := !current.equal(desired)
	if changed {
		if err := principal.Authorize(access.Keys); err != nil {
			return false, err
		}
		if _, err := WritePolicies(ctx, tx, changes[0].ProjectID, desired); err != nil {
			return false, err
		}
	}
	restChanged, err := ApplyPolicies(ctx, tx, principal, changes[1:])
	return changed || restChanged, err
}
