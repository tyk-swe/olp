package access

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

func (s *Server) deleteBudgetGroup(r *http.Request, _ Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "budget_group_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	claim, replayed, err := s.Replay(r, tx, p, map[string]string{"budget_group_id": id})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if replayed != nil {

		var projectID *string
		if err := tx.QueryRow(r.Context(), "SELECT project_id::text FROM olp.budget_groups WHERE id=$1", id).Scan(&projectID); err != nil {
			return httpkit.Reply{}, err
		}
		if err := p.Project(projectID, Change); err != nil {
			return httpkit.Reply{}, err
		}

		return *replayed, tx.Commit(r.Context())
	}
	var etag string
	var projectID *string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text,project_id::text FROM olp.budget_groups WHERE id=$1 AND retired_at IS NULL FOR UPDATE", id).Scan(&etag, &projectID); err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(projectID, Change); err != nil {
		return httpkit.Reply{}, err
	}
	if err = httpkit.Match(r, etag); err != nil {
		return httpkit.Reply{}, err
	}

	err = retireBudgetGroup(r.Context(), tx, id)

	if err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23503" {
			return httpkit.Reply{}, httpkit.Fail(409, "budget_group_in_use", "The resource still has dependent resources or retained history.")
		}
		return httpkit.Reply{}, err
	}

	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return httpkit.Reply{}, err
	}

	if err = AuditForProject(r.Context(), tx, r, p.Actor(), "budget_group.delete", "budget_group", id, "success", projectID); err != nil {
		return httpkit.Reply{}, err
	}
	result := httpkit.Reply{Status: http.StatusNoContent}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return httpkit.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}
