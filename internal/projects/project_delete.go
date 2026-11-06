package projects

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
)

// Deletion removes an empty project boundary. PostgreSQL foreign keys retain
// provider, route, key and accounting history until those dependencies permit it.
func (s *Server) deleteProject(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "project_id")
	if err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	claim, replayed, err := s.access.Replay(r, tx, p, map[string]string{"project_id": id})
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return *replayed, tx.Commit(r.Context())
	}
	var etag string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text FROM olp.projects WHERE id=$1 FOR UPDATE", id).Scan(&etag); err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	var used bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.providers WHERE project_id=$1)
	 OR EXISTS(SELECT 1 FROM olp.routes WHERE project_id=$1)
	 OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE project_id=$1)
	 OR EXISTS(SELECT 1 FROM olp.api_keys WHERE project_id=$1)`, id).Scan(&used); err != nil {
		return access.Reply{}, err
	}
	if used {
		return access.Reply{}, access.Fail(http.StatusConflict, "project_in_use", "Project still has resources or retained accounting history.")
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.projects WHERE id=$1", id); err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23503" {
			return access.Reply{}, access.Fail(http.StatusConflict, "project_in_use", "Project still has resources or retained accounting history.")
		}
		return access.Reply{}, err
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "project.delete", "project", id, "success"); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: http.StatusNoContent}
	if err = s.access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return result, tx.Commit(r.Context())
}
