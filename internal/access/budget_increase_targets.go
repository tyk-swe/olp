package access

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits"
)

// BudgetTarget identifies a single accounting boundary, never a template shared
// by other members. Its identifiers are metadata, not raw end-user identities.
type BudgetTarget struct {
	Kind          string `json:"kind"`
	ID            string `json:"id,omitempty"`
	Route         string `json:"route,omitempty"`
	EndUserDigest string `json:"end_user_digest,omitempty"`
	Label         string `json:"label,omitempty"`
	Value         string `json:"value,omitempty"`
}

func (t *BudgetTarget) validate() error {
	switch t.Kind {
	case "installation", "organization", "project", "api_key", "budget_group", "key_route", "key_end_user", "project_end_user", "attribution":
	default:
		return Invalid("target.kind", "Choose a supported budget boundary.")
	}
	if t.Kind != "installation" {
		id, err := uuid.Parse(t.ID)
		if err != nil {
			return Invalid("target.id", "A resource UUID is required.")
		}
		t.ID = id.String()
	} else if t.ID != "" {
		return Invalid("target.id", "Installation targets have no resource ID.")
	}
	if (t.Kind == "key_route" && !RouteSlug.MatchString(t.Route)) || (t.Kind != "key_route" && t.Route != "") {
		return Invalid("target.route", "Supply a route only for a key-route budget.")
	}
	identified := t.Kind == "key_end_user" || t.Kind == "project_end_user"
	if (identified && !ValidEndUserDigest(t.EndUserDigest)) || (!identified && t.EndUserDigest != "") {
		return Invalid("target.end_user_digest", "Supply a digest only for an end-user budget.")
	}
	if t.Kind == "attribution" {
		if !attribution.KeyPattern.MatchString(t.Label) || !attribution.ValuePattern.MatchString(t.Value) {
			return Invalid("target", "Supply a valid attribution label and value.")
		}
	} else if t.Label != "" || t.Value != "" {
		return Invalid("target", "Labels belong only to attribution budgets.")
	}
	return nil
}

func costs(l AdmissionLimits) *BudgetPolicy {
	return &BudgetPolicy{DailyCostLimit: l.DailyCostLimit, WeeklyCostLimit: l.WeeklyCostLimit, MonthlyCostLimit: l.MonthlyCostLimit}
}

func (s *Server) budgetIncreaseTarget(r *http.Request, tx pgx.Tx, p Principal, t BudgetTarget) (owner string, project *string, policy *BudgetPolicy, err error) {
	if err = t.validate(); err != nil {
		return
	}
	if t.Kind == "installation" {
		if err = p.Authorize(Settings); err != nil {
			return
		}
		owner = limits.AggregateBudgetID("installation", s.Installation)
		err = tx.QueryRow(r.Context(), "SELECT budget_policy FROM olp.installation WHERE singleton").Scan(&policy)
		return
	}
	if err = p.Authorize(Keys); err != nil {
		return
	}
	switch t.Kind {
	case "organization":
		if err = p.Authorize(ManageOrganization); err != nil {
			return
		}
		if err = p.Organization(t.ID, Change); err != nil {
			return
		}
		owner = limits.AggregateBudgetID("organization", t.ID)
		err = tx.QueryRow(r.Context(), "SELECT budget_policy FROM olp.organizations WHERE id=$1", t.ID).Scan(&policy)
		return
	case "project", "project_end_user", "attribution":
		project = &t.ID
		if err = p.Project(project, Change); err != nil {
			return
		}
		var endUser *EndUserPolicy
		var templates LimitTemplates
		var labels AttributionBudgets
		if err = tx.QueryRow(r.Context(), "SELECT budget_policy,end_user_policy,limit_templates,attribution_budgets FROM olp.projects WHERE id=$1", t.ID).Scan(&policy, &endUser, &templates, &labels); err != nil {
			return
		}
		switch t.Kind {
		case "project":
			owner = limits.AggregateBudgetID("project", t.ID)
		case "attribution":
			owner = limits.AttributionBudgetID(t.ID, t.Label, t.Value)
			v := labels[t.Label][t.Value]
			policy = &v
		case "project_end_user":
			owner = limits.EndUserProjectID(t.ID, t.EndUserDigest)
			endUser, err = templates.endUser(endUser)
			if err == nil {
				policy = costs(endUser.Limits(t.EndUserDigest))
			}
		}
	case "budget_group":
		var l AdmissionLimits
		err = tx.QueryRow(r.Context(), "SELECT project_id::text,effective_limits FROM olp.budget_groups_with_limits WHERE id=$1", t.ID).Scan(&project, &l)
		owner = t.ID
		policy = costs(l)
	default:
		var a Authority
		var templates LimitTemplates
		err = tx.QueryRow(r.Context(), `SELECT k.project_id::text,k.policy,COALESCE(p.limit_templates,'{}'::jsonb) FROM olp.api_keys k LEFT JOIN olp.projects p ON p.id=k.project_id WHERE k.id=$1 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>now())`, t.ID).Scan(&a.ProjectID, &a.Policy, &templates)
		if err != nil {
			return
		}
		project = a.ProjectID
		if err = p.Project(project, Change); err != nil {
			return
		}
		if err = a.BindLimitTemplates(templates); err != nil {
			return
		}
		switch t.Kind {
		case "api_key":
			owner = t.ID
			policy = costs(a.Policy.AdmissionLimits())
		case "key_route":
			owner = limits.KeyRouteBudgetID(t.ID, t.Route)
			policy = costs(a.Policy.RouteLimits[t.Route])
		case "key_end_user":
			owner = limits.EndUserKeyID(t.ID, t.EndUserDigest)
			policy = costs(a.Policy.EndUserPolicy.Limits(t.EndUserDigest))
		}
	}
	if err == nil {
		err = p.Project(project, Change)
	}
	return
}
