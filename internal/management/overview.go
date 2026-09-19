package management

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

// Overview serves the console's aggregate readiness counts so the overview
// page does not paginate full collections just to count them.
type Overview struct {
	Access *access.Server
}

// Register mounts the overview summary on the management surface.
func (o *Overview) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v3/overview", o.Access.Handle(o.summary))
}

func (o *Overview) summary(r *http.Request) (access.Reply, error) {
	if _, err := o.Access.Principal(r, o.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	var response contract.OverviewResponse
	err := o.Access.Pool.QueryRow(r.Context(),
		"SELECT "+
			"(SELECT count(*) FROM olp_go.providers WHERE active_revision IS NOT NULL),"+
			"(SELECT count(*) FROM olp_go.routes),"+
			"(SELECT count(*) FROM olp_go.provider_models WHERE enabled),"+
			"EXISTS(SELECT 1 FROM olp_go.api_keys WHERE revoked_at IS NULL)").
		Scan(&response.ActiveProviders, &response.ActiveRoutes, &response.EnabledModels, &response.UsableApiKey)
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(response), nil
}
