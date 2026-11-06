package access

import (
	"context"
	"net/http"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
)

func checkKeyRouteGroups(r *http.Request, q Queryer, names []string, projectID *string) error {
	return checkRouteGroupNames(r.Context(), q, names, projectID)
}
func checkRouteGroupNames(ctx context.Context, q Queryer, names []string, projectID *string) error {
	if len(names) == 0 {
		return nil
	}
	if projectID == nil {
		return Invalid("allowed_route_groups", "Route groups require a project-scoped key.")
	}
	var groups keyauthority.RouteGroups
	if err := q.QueryRow(ctx, "SELECT route_groups FROM olp.projects WHERE id=$1", *projectID).Scan(&groups); err != nil {
		return err
	}
	for _, name := range names {
		if _, ok := groups[name]; !ok {
			return Invalid("allowed_route_groups", "Every referenced group must exist in the key's project.")
		}
	}
	return nil
}
