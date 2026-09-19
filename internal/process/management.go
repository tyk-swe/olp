package process

import (
	"log/slog"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func registerManagement(mux *http.ServeMux, control *access.Server, policy *egress.Policy, limiter *limits.Limiter, rt *runtime.Manager, gw *gateway.Server, cache *observability.Cache, log *slog.Logger) {
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
	(&media.Management{Access: control, Pool: control.Pool}).Register(mux)
	(&management.Overview{Access: control}).Register(mux)
	(&observability.Management{Access: control, Cache: cache, Pool: control.Pool}).Register(mux)
	// Usage, pricing, request history and recovery reporting are part
	// of the management surface; their patterns are more specific than
	// its catch-all, which answers everything no surface claims.
	(&usage.Server{Access: control, VendorKind: providers.VendorKind}).Register(mux)
}
