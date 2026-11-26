package manage

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/resources"
)

type API struct {
	server      *access.Server
	settlements *resources.SettlementStore
}

func New(server *access.Server) *API {
	var settlements *resources.SettlementStore
	if server != nil {
		settlements = resources.NewSettlementStore(server.Pool)
	}
	return &API{server: server, settlements: settlements}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/provider-resources", a.list)
	a.server.Route(mux, "GET /api/v1/provider-resource-contracts", a.contracts)
	a.server.Route(mux, "POST /api/v1/provider-resources/{id}/reconcile", a.reconcile)
	a.server.Route(mux, "POST /api/v1/provider-resources/{id}/usage-evidence", a.usageEvidence)
}
