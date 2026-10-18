package routes

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
)

type inspectionKeyContext struct {
	id, reason         string
	allowProviderState bool
	authority          access.Authority
}

func (s *Server) inspectionKey(r *http.Request, q access.Queryer, principal access.Principal, selected *string, route routing.Route) (inspectionKeyContext, error) {
	key := inspectionKeyContext{}
	if selected == nil {
		if err := principal.Project(route.ProjectID, access.View); err != nil {
			return key, err
		}
		key.authority.ProjectID = route.ProjectID
		err := q.QueryRow(r.Context(), `SELECT p.budget_policy,p.organization_id::text,o.budget_policy,
COALESCE(p.attribution_budgets,'{}'::jsonb),COALESCE(p.attribution_policy,'{}'::jsonb),i.budget_policy
FROM olp.installation i LEFT JOIN olp.projects p ON p.id=$1::uuid
LEFT JOIN olp.organizations o ON o.id=p.organization_id WHERE i.singleton`, route.ProjectID).Scan(
			&key.authority.ProjectBudget, &key.authority.OrganizationID, &key.authority.OrganizationBudget,
			&key.authority.ProjectAttributionBudgets, &key.authority.ProjectAttributionPolicy, &key.authority.InstallationBudget)
		return key, err
	}
	var err error
	key.id, err = access.ParseUUID(*selected)
	if err != nil {
		return key, access.Invalid("api_key_id", "Use an API key identifier.")
	}
	var authority access.Authority
	var raw []byte
	var groups access.RouteGroups
	var templates access.LimitTemplates
	if err := q.QueryRow(r.Context(), `SELECT k.policy,k.expires_at,k.revoked_at,k.project_id::text,
COALESCE(p.route_groups,'{}'::jsonb),COALESCE(p.limit_templates,'{}'::jsonb),
k.budget_group_id::text,g.limit_template,g.requests_per_minute,g.tokens_per_minute,g.max_concurrency,
g.daily_cost_limit::text,g.weekly_cost_limit::text,g.monthly_cost_limit::text,COALESCE(g.cost_basis,'cost'),
p.end_user_policy,COALESCE(p.attribution_policy,'{}'::jsonb),p.budget_policy,COALESCE(p.attribution_budgets,'{}'::jsonb),
p.organization_id::text,o.budget_policy,(SELECT budget_policy FROM olp.installation WHERE singleton)
FROM olp.api_keys k LEFT JOIN olp.budget_groups g ON g.id=k.budget_group_id
LEFT JOIN olp.projects p ON p.id=k.project_id LEFT JOIN olp.organizations o ON o.id=p.organization_id
WHERE k.id=$1`, key.id).Scan(&raw, &authority.ExpiresAt, &authority.RevokedAt, &authority.ProjectID,
		&groups, &templates, &authority.BudgetGroupID, &authority.BudgetGroupTemplate, &authority.BudgetGroupRPM,
		&authority.BudgetGroupTPM, &authority.BudgetGroupConcurrency, &authority.BudgetGroupDailyCostLimit,
		&authority.BudgetGroupWeeklyCostLimit, &authority.BudgetGroupMonthlyCostLimit, &authority.BudgetGroupCostBasis,
		&authority.ProjectEndUserPolicy, &authority.ProjectAttributionPolicy, &authority.ProjectBudget,
		&authority.ProjectAttributionBudgets, &authority.OrganizationID, &authority.OrganizationBudget, &authority.InstallationBudget); err != nil {
		return key, err
	}
	if err := principal.Project(authority.ProjectID, access.View); err != nil {
		return key, err
	}
	if !sameProject(authority.ProjectID, route.ProjectID) {
		return key, access.Invalid("api_key_id", "The API key and route must belong to the same project.")
	}
	if err := json.Unmarshal(raw, &authority.Policy); err != nil {
		return key, err
	}
	authority.Policy = authority.Policy.InRegion(s.Region)
	if err := authority.BindRouteGroups(groups); err != nil {
		return key, err
	}
	if authority.RevokedAt == nil {
		if err := authority.BindLimitTemplates(templates); err != nil {
			return key, err
		}
	}
	key.allowProviderState = authority.Policy.AllowProviderState
	key.authority = authority
	if !authority.Allows("inference", route.Slug, route.ProjectID, time.Now()) {
		key.reason = "api_key_not_authorized"
		if allowed := authority.RouteAllowlist(); allowed != nil && !slices.Contains(allowed, route.Slug) {
			key.reason = "route_not_allowed_for_key"
		}
	}
	return key, nil
}

func applyInspectionKeyReason(decisions []routing.Decision, reason string) {
	if reason == "" {
		return
	}
	for i := range decisions {
		decisions[i].Eligible = false
		decisions[i].Attempt = nil
		decisions[i].Reason = &reason
	}
}
