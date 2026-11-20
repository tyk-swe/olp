package budgets

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
)

func (a *API) deleteGroup(r *http.Request, caller authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "budget_group_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := a.managesGroup(r, caller, id); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Remove(a.server, r, map[string]string{"budget_group_id": id}, func(tx pgx.Tx, p authz.Principal) (*string, error) {
		var etag string
		var projectID *string
		if err := tx.QueryRow(r.Context(), "SELECT etag::text,project_id::text FROM olp.budget_groups WHERE id=$1 AND retired_at IS NULL FOR UPDATE", id).Scan(&etag, &projectID); err != nil {
			return nil, err
		}
		if err := p.Project(projectID, authz.Change); err != nil {
			return nil, err
		}
		return projectID, httpkit.Match(r, etag)
	}, func(tx pgx.Tx, _ authz.Principal, projectID *string) (httpkit.Reply, access.Event, error) {
		if err := retireBudgetGroup(r.Context(), tx, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		_, err := access.AdvanceAuthority(r.Context(), tx)
		return httpkit.Reply{Status: http.StatusNoContent}, access.Event{Action: "budget_group.delete", Resource: "budget_group", ID: id, Project: projectID}, err
	})
}

// A replayed delete returns its stored reply before the row is locked, and the
// retired row no longer matches the lock query, so the project check for that
// path is made here.
func (a *API) managesGroup(r *http.Request, caller authz.Principal, id string) error {
	var projectID *string
	if err := a.server.Pool.QueryRow(r.Context(), "SELECT project_id::text FROM olp.budget_groups WHERE id=$1", id).Scan(&projectID); err != nil {
		return err
	}
	return caller.Project(projectID, authz.Change)
}
