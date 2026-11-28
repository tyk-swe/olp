package main

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/mcp/managementtools"
	"github.com/tyk-swe/olp/internal/notifications"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/promotion/project"
	promotionserve "github.com/tyk-swe/olp/internal/promotion/serve"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/usage/pricebook"
)

func (m Management) registerOperations(mux *http.ServeMux) {
	management.Register(mux)
	managementtools.NewMCP(m.Access, mux).Register(mux)
	notifications.NewManagement(m.Access, *m.core.policy).Register(mux)
	(&management.Overview{Access: m.Access}).Register(mux)
	(&observability.Management{Access: m.Access, Cache: m.health, Pool: m.Access.Pool, Fleet: m.fleetHealth()}).Register(mux)
}

func (m Management) registerPromotion(mux *http.ServeMux, providerServer *providers.Server) {
	promotionserve.Register(mux, promotionserve.Deps{
		Access:     m.Access,
		SignIn:     m.Identity,
		Egress:     m.core.policy,
		Unconfined: m.core.unconfined,
		Identity:   m.IdentityClient,
		Costs:      project.CostsOf(m.stores.limiter),
		Normalizer: pricebook.NewNormalizer(vendors.Kind),
		Drafts:     providerServer,
	})
}
