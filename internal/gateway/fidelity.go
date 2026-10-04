package gateway

import (
	"context"
	"github.com/tyk-swe/olp/internal/runtime"
	"net/http"
)

func (s *Server) checkRouteFidelity(ctx context.Context, route *runtime.Route) *Error {
	if route.Fidelity.Strict() {
		if err := s.Runtime.CheckRouteFidelity(ctx, *route); err != nil {
			return serverError(http.StatusServiceUnavailable, "route_fidelity_unavailable", "The current strict route contract could not be confirmed. Retry after the gateway refreshes its configuration.")
		}
	}
	return nil
}
