package access

import (
	"context"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/database"
)

// RequireProject checks that p may place a new resource in projectID. A
// project that does not exist answers like one outside p's scope, and the
// unassigned boundary takes an installation-wide principal.
func (s *Server) RequireProject(ctx context.Context, q database.Queryer, p authz.Principal, projectID *string) error {
	if projectID == nil {
		if p.AllProjects {
			return nil
		}
		return httpkit.Forbidden()
	}
	if err := p.Project(projectID, authz.View); err != nil {
		return err
	}
	var exists bool
	if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.projects WHERE id=$1)", *projectID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return httpkit.NotFound
	}
	return p.Project(projectID, authz.Change)
}
