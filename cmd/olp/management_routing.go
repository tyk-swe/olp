package main

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/cache/cacheadmin"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routes/modelcatalog"
)

func (m Management) registerRouting(mux *http.ServeMux) *routes.Server {
	(&release.DelegationManagement{Access: m.Access, Manager: m.Runtime}).Register(mux)
	release.RegisterGenerations(m.Access, mux)
	cacheadmin.NewAPI(m.Access, m.ResponseCache).Register(mux)
	(&modelcatalog.Server{Access: m.Access, Runtime: m.Runtime, AuthenticateKey: m.catalogAuthority, Origin: m.Access.Origin}).Register(mux)
	routeServer := &routes.Server{
		Access: m.Access, InlineMedia: m.Gateway.InlineMediaLimits(),
		Workflows: m.Gateway.WorkflowResolver(), WorkflowRelease: m.Runtime.Release,
		Inputs: m.Runtime.RoutingInputs, Region: m.Runtime.Region(),
		UnconfinedPlugins: m.Unconfined != nil, Catalog: m.Catalog,
	}
	if m.Limits != nil {
		routeServer.Supply = quota.NewAdmission(m.Limits.Limiter, m.Limits.Fleet, nil, m.Log)
		routeServer.Fleet = m.Limits.Fleet
	}
	routeServer.Register(mux)
	return routeServer
}

func (m Management) catalogAuthority(r *http.Request) (keyauthority.Authority, error) {
	if m.Gateway == nil {
		return keyauthority.Authority{}, httpkit.Fail(503, "authority_unavailable", "Key authority is unavailable.")
	}
	authority, failure := m.Gateway.Gate.Authenticate(r, "models_read")
	if failure != nil {
		return keyauthority.Authority{}, httpkit.Fail(failure.Status, failure.Code, failure.Message)
	}
	return authority, nil
}
