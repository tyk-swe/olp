package process

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/configuration"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/signing"
	"github.com/tyk-swe/olp/internal/usage"
)

// Management is everything the management API is composed from. Processes
// and integration tests mount it the same way, so neither can drift from the
// routes the contract declares.
type Management struct {
	Access        *access.Server
	Egress        *egress.Policy
	Limiter       *limits.Limiter
	Runtime       *runtime.Manager
	Gateway       *gateway.Server
	Media         *media.Service
	Health        *observability.Cache
	Log           *slog.Logger
	PluginRuntime *plugins.Runtime
	PluginHost    *plugins.Host
	Unconfined    *plugins.Unconfined
	// Catalog is the reference catalog this release ships, verified at
	// start-up.
	Catalog *catalog.Signed
}

// Register mounts the whole management API: the published contract, every
// feature's routes, and the catch-all that answers 404 for the rest.
func (m Management) Register(mux *http.ServeMux) {
	management.Register(mux)
	m.Access.Egress = m.Egress
	m.Access.Register(mux)
	catalogue := providers.New(m.Access, m.Egress, m.PluginHost)
	catalogue.Unconfined = m.Unconfined
	catalogue.Log = m.Log
	catalogue.Plugins = m.PluginHost
	catalogue.Catalog = m.Catalog
	if m.Limiter != nil {
		catalogue.Quotas = m.Limiter
	}
	catalogue.Register(mux)
	routeServer := routes.New(m.Access)
	routeServer.Inputs = m.Runtime.RoutingInputs
	routeServer.UnconfinedPlugins = m.Unconfined != nil
	routeServer.Catalog = m.Catalog
	routeServer.Register(mux)
	(&gateway.Playground{Access: m.Access, Gateway: m.Gateway}).Register(mux)
	(&media.Management{Access: m.Access, Pool: m.Access.Pool, Jobs: m.Media, Log: m.Log}).Register(mux)
	(&resources.Management{Access: m.Access, Pool: m.Access.Pool}).Register(mux)
	(&management.Overview{Access: m.Access}).Register(mux)
	(&observability.Management{Access: m.Access, Cache: m.Health, Pool: m.Access.Pool}).Register(mux)
	(&plugins.Management{Access: m.Access, Runtime: m.PluginRuntime, Host: m.PluginHost, Unconfined: m.Unconfined}).Register(mux)
	// Usage, pricing, request history and recovery reporting are part
	// of the management surface; their patterns are more specific than
	// its catch-all, which answers everything no surface claims.
	(&usage.Server{Access: m.Access, VendorKind: providers.VendorKind, Egress: m.Egress, Catalog: m.Catalog, CatalogKeys: signing.Trusted()}).Register(mux)
	(&configuration.Server{
		Access:                 m.Access,
		Egress:                 m.Egress,
		Unconfined:             m.Unconfined,
		VendorKind:             providers.VendorKind,
		StoreNetworkCredential: catalogue.StoreNetworkCredential,
		StoreCredential: func(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error) {
			id, _, err := catalogue.StoreCredential(ctx, tx, providerID, secret)
			return id, err
		},
	}).Register(mux)
}
