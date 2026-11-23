package projects

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
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

func (s *Server) projectAttributionBudgets(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&id, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return readAttributionBudgets(r, s.access.Pool, id)
}

func readAttributionBudgets(r *http.Request, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (httpkit.Reply, error) {
	var budgets, usage json.RawMessage
	var etag string
	if err := q.QueryRow(r.Context(), `SELECT attribution_budgets,etag::text,`+ledger.AttributionBudgetsSQL("p")+` FROM olp.projects p WHERE id=$1`, id).Scan(&budgets, &etag, &usage); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(map[string]any{"budgets": budgets, "usage": usage, "etag": etag}, etag), nil
}

func (s *Server) putProjectAttributionBudgets(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input struct {
		Budgets authority.AttributionBudgets `json:"budgets"`
	}
	if err = httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Budgets == nil {
		return httpkit.Reply{}, httpkit.Invalid("budgets", "Supply a budget map; an empty object clears caps without resetting spend.")
	}
	if err = input.Budgets.ValidateAndNormalize(); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err = p.Project(&id, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		current, etag, err := ReadPolicies(r.Context(), tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		_, err = WritePolicies(r.Context(), tx, id, Policies{EndUser: current.EndUser, Attribution: current.Attribution, RouteGroups: current.RouteGroups, Budget: current.Budget, Templates: current.Templates, AttributionBudgets: input.Budgets})
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply, err := readAttributionBudgets(r, tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return reply, access.Event{Action: "project.attribution_budgets.update", Resource: "project", ID: id}, nil
	})
}
