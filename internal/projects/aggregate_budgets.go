package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/settings"
)

func (s *Server) projectBudget(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.readAggregateBudget(r, p, true)
}

func (s *Server) installationBudget(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.readAggregateBudget(r, p, false)
}

func (s *Server) putProjectBudget(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.writeAggregateBudget(r, p, true)
}

func (s *Server) putInstallationBudget(r *http.Request, p access.Principal) (access.Reply, error) {
	return s.writeAggregateBudget(r, p, false)
}

// Table names are selected internally, never supplied by the request.
func (s *Server) budgetBoundary(r *http.Request, p access.Principal, project, change bool) (id, table, etagColumn, level string, err error) {
	id, table, etagColumn, level = s.access.Installation, "olp.installation", "budget_etag", "installation"
	if r.PathValue("organization_id") != "" {
		table, etagColumn, level = "olp.organizations", "etag", "organization"
		id, err = access.IDParam(r, "organization_id")
		if err != nil {
			return
		}
		need := access.View
		if change {
			need = access.Change
		}
		err = p.Organization(id, need)
		return
	}
	if !project {
		return
	}
	table, etagColumn, level = "olp.projects", "etag", "project"
	id, err = access.IDParam(r, "project_id")
	if err != nil {
		return
	}
	need := access.View
	if change {
		need = access.Change
	}
	err = p.Project(&id, need)
	return
}

func (s *Server) readAggregateBudget(r *http.Request, p access.Principal, project bool) (access.Reply, error) {
	id, table, column, level, err := s.budgetBoundary(r, p, project, false)
	if err != nil {
		return access.Reply{}, err
	}
	var policy, usage json.RawMessage
	var etag string
	err = s.access.Pool.QueryRow(r.Context(), "SELECT COALESCE(budget_policy,'null'::jsonb),"+column+"::text,"+aggregateReportSQL(level)+" FROM "+table+" p WHERE id=$1", id).Scan(&policy, &etag, &usage)
	return access.Detail(map[string]any{"policy": policy, "etag": etag, "usage": usage}, etag), err
}

func (s *Server) writeAggregateBudget(r *http.Request, _ access.Principal, project bool) (access.Reply, error) {
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if err := access.Decode(r, &body); err != nil {
		return access.Reply{}, err
	}
	if len(body.Policy) == 0 {
		return access.Reply{}, access.Invalid("policy", "A policy or explicit null is required.")
	}
	var policy *authority.BudgetPolicy
	d := json.NewDecoder(bytes.NewReader(body.Policy))
	d.DisallowUnknownFields()
	if err := d.Decode(&policy); err != nil {
		return access.Reply{}, access.Invalid("policy", "The budget policy is invalid.")
	}
	if err := policy.ValidateAndNormalize(); err != nil {
		return access.Reply{}, err
	}
	if !policy.Limited() {
		policy = nil
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	id, table, column, level, err := s.budgetBoundary(r, p, project, true)
	if err != nil {
		return access.Reply{}, err
	}
	var etag string
	if err = tx.QueryRow(r.Context(), "SELECT "+column+"::text FROM "+table+" WHERE id=$1 FOR UPDATE", id).Scan(&etag); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	if level == "project" {
		current, _, readErr := ReadPolicies(r.Context(), tx, id)
		if readErr != nil {
			return access.Reply{}, readErr
		}
		etag, err = WritePolicies(r.Context(), tx, id, Policies{EndUser: current.EndUser, Attribution: current.Attribution, RouteGroups: current.RouteGroups, Budget: policy, Templates: current.Templates, AttributionBudgets: current.AttributionBudgets})
		if err != nil {
			return access.Reply{}, err
		}
	} else if level == "organization" {
		if etag, err = SetOrganizationBudget(r.Context(), tx, id, policy); err != nil {
			return access.Reply{}, err
		}
	} else if etag, err = settings.SetInstallationBudget(r.Context(), tx, id, policy); err != nil {
		return access.Reply{}, err
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), level+".budget.update", level, id, "success"); err != nil {
		return access.Reply{}, err
	}
	var usage json.RawMessage
	if err = tx.QueryRow(r.Context(), "SELECT "+aggregateReportSQL(level)+" FROM "+table+" p WHERE id=$1", id).Scan(&usage); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"policy": policy, "etag": etag, "usage": usage}, etag), tx.Commit(r.Context())
}

func aggregateReportSQL(level string) string {
	if level == "organization" {
		return limits.OrganizationBudgetSQL
	}
	return limits.AggregateBudgetSQL(level == "project")
}

func SetOrganizationBudget(ctx context.Context, tx pgx.Tx, id string, policy *authority.BudgetPolicy) (string, error) {
	if policy.Limited() {
		if err := limits.EnsureAggregateBudget(ctx, tx, "organization", id); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	etag := access.NewID()
	if _, err := tx.Exec(ctx, "UPDATE olp.organizations SET budget_policy=NULLIF($2::jsonb,'null'::jsonb),etag=$3,updated_at=now() WHERE id=$1", id, encoded, etag); err != nil {
		return "", err
	}
	return etag, nil
}
