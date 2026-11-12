package gateway

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) checkRouteFidelity(ctx context.Context, route *routing.Route) *fault.Error {
	if route.Fidelity.Strict() {
		if err := s.deps.Runtime.CheckRouteFidelity(ctx, *route); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "route_fidelity_unavailable", "The current strict route contract could not be confirmed. Retry after the gateway refreshes its configuration.")
		}
	}
	return nil
}
