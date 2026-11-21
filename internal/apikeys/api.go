package apikeys

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

type API struct {
	server   *access.Server
	overlaps *OverlapRecorder
}

func NewAPI(server *access.Server, overlaps *OverlapRecorder) *API {
	return &API{server: server, overlaps: overlaps}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/api-keys", a.apiKeys)
	a.server.Route(mux, "POST /api/v1/api-keys", a.createAPIKey)
	a.server.Route(mux, "GET /api/v1/api-keys/{api_key_id}", a.apiKey)
	a.server.Route(mux, "PATCH /api/v1/api-keys/{api_key_id}", a.updateAPIKey)
	a.server.Route(mux, "POST /api/v1/api-keys/{api_key_id}/revoke", a.revokeAPIKey)
	a.server.Route(mux, "POST /api/v1/api-keys/{api_key_id}/rotate", a.rotateAPIKey)
	a.server.Route(mux, "POST /api/v1/api-keys/{api_key_id}/end-user", a.lookupEndUser)
	a.server.Route(mux, "GET /api/v1/workload-issuers", a.workloadIssuers)
	a.server.Route(mux, "POST /api/v1/workload-issuers", a.createWorkloadIssuer)
	a.server.Route(mux, "GET /api/v1/workload-issuers/{issuer_id}", a.workloadIssuer)
	a.server.Route(mux, "PUT /api/v1/workload-issuers/{issuer_id}", a.putWorkloadIssuer)
}
