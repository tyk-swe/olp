package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

func (s *Server) projectBudget(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.readAggregateBudget(r, p, true)
}

func (s *Server) installationBudget(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.readAggregateBudget(r, p, false)
}

func (s *Server) putProjectBudget(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.writeAggregateBudget(r, p, true)
}

func (s *Server) putInstallationBudget(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return s.writeAggregateBudget(r, p, false)
}

// Table names are selected internally, never supplied by the request.
func (s *Server) budgetBoundary(r *http.Request, p authz.Principal, project, change bool) (id, table, etagColumn, level string, err error) {
	id, table, etagColumn, level = s.access.Installation, "olp.installation", "budget_etag", "installation"
	if r.PathValue("organization_id") != "" {
		table, etagColumn, level = "olp.organizations", "etag", "organization"
		id, err = httpkit.IDParam(r, "organization_id")
		if err != nil {
			return
		}
		need := authz.View
		if change {
			need = authz.Change
		}
		err = p.Organization(id, need)
		return
	}
	if !project {
		return
	}
	table, etagColumn, level = "olp.projects", "etag", "project"
	id, err = httpkit.IDParam(r, "project_id")
	if err != nil {
		return
	}
	need := authz.View
	if change {
		need = authz.Change
	}
	err = p.Project(&id, need)
	return
}

func (s *Server) readAggregateBudget(r *http.Request, p authz.Principal, project bool) (httpkit.Reply, error) {
	id, table, column, level, err := s.budgetBoundary(r, p, project, false)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var policy, usage json.RawMessage
	var etag string
	err = s.access.Pool.QueryRow(r.Context(), "SELECT COALESCE(budget_policy,'null'::jsonb),"+column+"::text,"+aggregateReportSQL(level)+" FROM "+table+" p WHERE id=$1", id).Scan(&policy, &etag, &usage)
	return httpkit.Detail(map[string]any{"policy": policy, "etag": etag, "usage": usage}, etag), err
}

func (s *Server) writeAggregateBudget(r *http.Request, _ authz.Principal, project bool) (httpkit.Reply, error) {
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if err := httpkit.Decode(r, &body); err != nil {
		return httpkit.Reply{}, err
	}
	if len(body.Policy) == 0 {
		return httpkit.Reply{}, httpkit.Invalid("policy", "A policy or explicit null is required.")
	}
	var policy *keyauthority.BudgetPolicy
	d := json.NewDecoder(bytes.NewReader(body.Policy))
	d.DisallowUnknownFields()
	if err := d.Decode(&policy); err != nil {
		return httpkit.Reply{}, httpkit.Invalid("policy", "The budget policy is invalid.")
	}
	if err := policy.ValidateAndNormalize(); err != nil {
		return httpkit.Reply{}, err
	}
	if !policy.Limited() {
		policy = nil
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		id, table, column, level, err := s.budgetBoundary(r, p, project, true)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var etag string
		if err = tx.QueryRow(r.Context(), "SELECT "+column+"::text FROM "+table+" WHERE id=$1 FOR UPDATE", id).Scan(&etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if level == "project" {
			current, _, readErr := ReadPolicies(r.Context(), tx, id)
			if readErr != nil {
				return httpkit.Reply{}, access.Event{}, readErr
			}
			etag, err = WritePolicies(r.Context(), tx, id, Policies{EndUser: current.EndUser, Attribution: current.Attribution, RouteGroups: current.RouteGroups, Budget: policy, Templates: current.Templates, AttributionBudgets: current.AttributionBudgets})
			if err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		} else if level == "organization" {
			if etag, err = SetOrganizationBudget(r.Context(), tx, id, policy); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		} else if etag, err = access.SetInstallationBudget(r.Context(), tx, id, policy); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var usage json.RawMessage
		if err = tx.QueryRow(r.Context(), "SELECT "+aggregateReportSQL(level)+" FROM "+table+" p WHERE id=$1", id).Scan(&usage); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(map[string]any{"policy": policy, "etag": etag, "usage": usage}, etag), access.Event{Action: level + ".budget.update", Resource: level, ID: id}, nil
	})
}

func aggregateReportSQL(level string) string {
	switch level {
	case "organization":
		return ledger.OrganizationBudgetSQL("p")
	case "project":
		return ledger.ProjectBudgetSQL("p")
	default:
		return ledger.InstallationBudgetSQL("p")
	}
}

func SetOrganizationBudget(ctx context.Context, tx pgx.Tx, id string, policy *keyauthority.BudgetPolicy) (string, error) {
	if policy.Limited() {
		if err := ledger.EnsureAggregateBudget(ctx, tx, "organization", id); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	etag := httpkit.NewID()
	if _, err := tx.Exec(ctx, "UPDATE olp.organizations SET budget_policy=NULLIF($2::jsonb,'null'::jsonb),etag=$3,updated_at=now() WHERE id=$1", id, encoded, etag); err != nil {
		return "", err
	}
	return etag, nil
}
