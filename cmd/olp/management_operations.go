package main

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/limits/admission"
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
	notifications.NewManagement(m.Access, *m.Egress).Register(mux)
	(&management.Overview{Access: m.Access}).Register(mux)
	health := &observability.Management{Access: m.Access, Cache: m.Health, Pool: m.Access.Pool}
	if m.Limits != nil {
		health.Fleet = m.Limits.Fleet
	}
	health.Register(mux)
}

func (m Management) registerPromotion(mux *http.ServeMux, providerServer *providers.Server) {
	promotionserve.Register(mux, promotionserve.Deps{
		Access:     m.Access,
		SignIn:     m.Identity,
		Egress:     m.Egress,
		Unconfined: m.Unconfined,
		Identity:   m.IdentityClient,
		Costs:      project.CostsOf(m.limiter()),
		Normalizer: pricebook.NewNormalizer(vendors.Kind),
		Drafts:     providerServer,
	})
}

func (m Management) limiter() *admission.Limiter {
	if m.Limits == nil {
		return nil
	}
	return m.Limits.Limiter
}
