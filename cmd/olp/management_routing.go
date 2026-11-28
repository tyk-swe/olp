package main

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/cache/cacheadmin"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routes/modelcatalog"
	"github.com/tyk-swe/olp/internal/routing"
)

func (m Management) registerRouting(mux *http.ServeMux) *routes.Server {
	(&release.DelegationManagement{Access: m.Access, Manager: m.core.rt}).Register(mux)
	release.RegisterGenerations(m.Access, mux)
	cacheadmin.NewAPI(m.Access, m.core.responseCache).Register(mux)
	(&modelcatalog.Server{Access: m.Access, Runtime: m.core.rt, AuthenticateKey: m.catalogAuthority, Origin: m.Access.Origin}).Register(mux)
	routeServer := &routes.Server{
		Access: m.Access, InlineMedia: m.serving.gateway.InlineMediaLimits(),
		Workflows: m.serving.gateway.WorkflowResolver(), WorkflowRelease: m.core.rt.Release,
		Inputs: m.core.rt.RoutingInputs, Region: m.core.rt.Region(),
		UnconfinedPlugins: m.core.unconfined != nil, Catalog: m.documents.catalog,
		Supply: m.routeSupply(), Fleet: m.fleetHealth(),
	}
	routeServer.Register(mux)
	return routeServer
}

func (m Management) routeSupply() routing.SupplyReader {
	shared := m.stores.shared
	if shared == nil {
		return nil
	}
	return quota.NewAdmission(shared.Limiter, shared.Fleet, nil, m.log)
}

func (m Management) catalogAuthority(r *http.Request) (keyauthority.Authority, error) {
	if m.serving.gateway == nil {
		return keyauthority.Authority{}, problem.Fail(503, "authority_unavailable", "Key authority is unavailable.")
	}
	authority, failure := m.serving.gateway.Gate.Authenticate(r, "models_read")
	if failure != nil {
		return keyauthority.Authority{}, problem.Fail(failure.Status, failure.Code, failure.Message)
	}
	return authority, nil
}
