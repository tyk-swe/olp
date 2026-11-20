package codeadmin

import (
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
)

type API struct{ server *access.Server }

func NewAPI(server *access.Server) *API { return &API{server: server} }

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/code/pools", a.pools)
	a.server.Route(mux, "POST /api/v1/code/pools", a.writePool)
	a.server.Route(mux, "PUT /api/v1/code/pools/{id}", a.writePool)
	a.server.Route(mux, "GET /api/v1/code/budgets", a.budgets)
	a.server.Route(mux, "POST /api/v1/code/budgets", a.writeBudget)
	a.server.Route(mux, "PUT /api/v1/code/budgets/{id}", a.writeBudget)
}

func (a *API) pools(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return List(a.server, r, p, poolJSON)
}

func (a *API) budgets(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	return List(a.server, r, p, `SELECT to_jsonb(x)-'created_by' FROM olp.code_token_budgets x`)
}

func (a *API) writePool(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var in PoolInput
	if err := httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	input, validationError := in.Validated()
	if validationError != nil {
		return httpkit.Reply{}, validationError
	}
	return Write(a.server, r, Pools, input.ProjectID, input, func(tx pgx.Tx, p authz.Principal, id, etag string, existing bool) (any, error) {
		return SavePool(r.Context(), tx, p, id, etag, input, existing)
	})
}

func (a *API) writeBudget(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var in BudgetInput
	if err := httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	input, validationError := in.Validated()
	if validationError != nil {
		return httpkit.Reply{}, validationError
	}
	return Write(a.server, r, Budgets, input.ProjectID, input, func(tx pgx.Tx, p authz.Principal, id, etag string, existing bool) (any, error) {
		return SaveBudget(r.Context(), tx, p, id, etag, input, existing)
	})
}
