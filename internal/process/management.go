package process

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/configuration"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// Management is everything the management API is composed from. Processes
// and integration tests mount it the same way, so neither can drift from the
// routes the contract declares.
type Management struct {
	Access  *access.Server
	Egress  *egress.Policy
	Limiter *limits.Limiter
	Runtime *runtime.Manager
	Gateway *gateway.Server
	Media   *media.Service
	Health  *observability.Cache
	Log     *slog.Logger
}

// Register mounts the whole management API: the published contract, every
// feature's routes, and the catch-all that answers 404 for the rest.
func (m Management) Register(mux *http.ServeMux) {
	control, policy, limiter, rt, gw, mediaJobs, cache, log := m.Access, m.Egress, m.Limiter, m.Runtime, m.Gateway, m.Media, m.Health, m.Log
	management.Register(mux)
	control.Egress = policy
	control.Register(mux)
	catalogue := providers.New(control, policy)
	catalogue.Log = log
	if limiter != nil {
		catalogue.Quotas = limiter
	}
	catalogue.Register(mux)
	routeServer := routes.New(control)
	routeServer.Inputs = rt.RoutingInputs
	routeServer.Register(mux)
	(&gateway.Playground{Access: control, Gateway: gw}).Register(mux)
	(&media.Management{Access: control, Pool: control.Pool, Jobs: mediaJobs, Log: log}).Register(mux)
	(&resources.Management{Access: control, Pool: control.Pool}).Register(mux)
	(&management.Overview{Access: control}).Register(mux)
	(&observability.Management{Access: control, Cache: cache, Pool: control.Pool}).Register(mux)
	// Usage, pricing, request history and recovery reporting are part
	// of the management surface; their patterns are more specific than
	// its catch-all, which answers everything no surface claims.
	(&usage.Server{Access: control, VendorKind: providers.VendorKind, Egress: policy}).Register(mux)
	(&configuration.Server{
		Access:                 control,
		Egress:                 policy,
		VendorKind:             providers.VendorKind,
		StoreNetworkCredential: catalogue.StoreNetworkCredential,
		StoreCredential: func(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error) {
			id, _, err := catalogue.StoreCredential(ctx, tx, providerID, secret)
			return id, err
		},
	}).Register(mux)
}
