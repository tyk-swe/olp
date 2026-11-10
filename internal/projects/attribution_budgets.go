package projects

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

func EnsureAttributionBudgetAccounts(ctx context.Context, tx pgx.Tx, project string, b authority.AttributionBudgets) error {
	for key, values := range b {
		for value := range values {
			if err := ledger.EnsureAttributionBudget(ctx, tx, project, key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) projectAttributionBudgets(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "project_id")
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&id, access.View); err != nil {
		return access.Reply{}, err
	}
	return readAttributionBudgets(r, s.access.Pool, id)
}

func readAttributionBudgets(r *http.Request, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (access.Reply, error) {
	var budgets, usage json.RawMessage
	var etag string
	if err := q.QueryRow(r.Context(), `SELECT attribution_budgets,etag::text,`+ledger.AttributionBudgetsSQL("p")+` FROM olp.projects p WHERE id=$1`, id).Scan(&budgets, &etag, &usage); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"budgets": budgets, "usage": usage, "etag": etag}, etag), nil
}

func (s *Server) putProjectAttributionBudgets(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "project_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input struct {
		Budgets authority.AttributionBudgets `json:"budgets"`
	}
	if err = access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if input.Budgets == nil {
		return access.Reply{}, access.Invalid("budgets", "Supply a budget map; an empty object clears caps without resetting spend.")
	}
	if err = input.Budgets.ValidateAndNormalize(); err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = p.Project(&id, access.Change); err != nil {
		return access.Reply{}, err
	}
	current, etag, err := ReadPolicies(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	_, err = WritePolicies(r.Context(), tx, id, Policies{EndUser: current.EndUser, Attribution: current.Attribution, RouteGroups: current.RouteGroups, Budget: current.Budget, Templates: current.Templates, AttributionBudgets: input.Budgets})
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "project.attribution_budgets.update", "project", id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply, err := readAttributionBudgets(r, tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	return reply, tx.Commit(r.Context())
}
