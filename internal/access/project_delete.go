package access

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
)

// Deletion removes an empty project boundary. PostgreSQL foreign keys retain
// provider, route, key and accounting history until those dependencies permit it.
func (s *Server) deleteProject(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, map[string]string{"project_id": id})
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	var etag string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text FROM olp.projects WHERE id=$1 FOR UPDATE", id).Scan(&etag); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	var used bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM olp.providers WHERE project_id=$1)
	 OR EXISTS(SELECT 1 FROM olp.routes WHERE project_id=$1)
	 OR EXISTS(SELECT 1 FROM olp.route_drafts WHERE project_id=$1)
	 OR EXISTS(SELECT 1 FROM olp.api_keys WHERE project_id=$1)`, id).Scan(&used); err != nil {
		return Reply{}, err
	}
	if used {
		return Reply{}, Fail(http.StatusConflict, "project_in_use", "Project still has resources or retained accounting history.")
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.projects WHERE id=$1", id); err != nil {
		var constraint *pgconn.PgError
		if errors.As(err, &constraint) && constraint.Code == "23503" {
			return Reply{}, Fail(http.StatusConflict, "project_in_use", "Project still has resources or retained accounting history.")
		}
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "project.delete", "project", id, "success"); err != nil {
		return Reply{}, err
	}
	result := Reply{Status: http.StatusNoContent}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}
