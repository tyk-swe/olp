package budgets

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

type API struct{ server *access.Server }

func NewAPI(server *access.Server) *API { return &API{server: server} }

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/budget-groups", a.groups)
	a.server.Route(mux, "POST /api/v1/budget-groups", a.createGroup)
	a.server.Route(mux, "GET /api/v1/budget-groups/{budget_group_id}", a.group)
	a.server.Route(mux, "PATCH /api/v1/budget-groups/{budget_group_id}", a.updateGroup)
	a.server.Route(mux, "DELETE /api/v1/budget-groups/{budget_group_id}", a.deleteGroup)
	a.server.Route(mux, "GET /api/v1/budget-increases", a.increases)
	a.server.Route(mux, "POST /api/v1/budget-increases", a.createIncrease)
	a.server.Route(mux, "GET /api/v1/budget-increases/{increase_id}", a.increase)
	a.server.Route(mux, "DELETE /api/v1/budget-increases/{increase_id}", a.revokeIncrease)
	a.server.Route(mux, "GET /api/v1/budget-increase-requests", a.increaseRequests)
	a.server.Route(mux, "POST /api/v1/budget-increase-requests", a.createIncreaseRequest)
	a.server.Route(mux, "GET /api/v1/budget-increase-requests/{request_id}", a.increaseRequestByID)
	a.server.Route(mux, "POST /api/v1/budget-increase-requests/{request_id}/approve", a.approveIncreaseRequest)
	a.server.Route(mux, "POST /api/v1/budget-increase-requests/{request_id}/decline", a.declineIncreaseRequest)
	a.server.Route(mux, "GET /api/v1/execution-debits", a.debits)
}
