package access

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/database"
)

func ValidateRouteReferences(r *http.Request, q database.Queryer, routes []string, projectID *string) error {
	if len(routes) == 0 {
		return nil
	}
	var mismatched bool
	if err := q.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r WHERE slug=ANY($1::text[]) AND project_id IS DISTINCT FROM $2::uuid)", routes, projectID).Scan(&mismatched); err != nil {
		return err
	}
	if mismatched {
		return httpkit.Invalid("allowed_routes", "Allowed routes must belong to the key's project.")
	}
	return nil
}
