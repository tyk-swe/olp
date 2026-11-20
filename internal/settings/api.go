package settings

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

type API struct{ server *access.Server }

func NewAPI(server *access.Server) *API { return &API{server: server} }

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/settings", a.list)
	a.server.Route(mux, "GET /api/v1/settings/{key}", a.get)
	a.server.Route(mux, "PUT /api/v1/settings/{key}", a.update)
	a.server.Public(mux, "GET /api/v1/branding", a.branding)
	a.server.Route(mux, "PUT /api/v1/branding", a.updateBranding, access.MaxBody(100<<10))
}
