package kernel

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) CheckRouteFidelity(ctx context.Context, route *routing.Route) *fault.Error {
	if route.Fidelity.Strict() {
		if err := s.Deps.Runtime.CheckRouteFidelity(ctx, *route); err != nil {
			return fault.ServerError(http.StatusServiceUnavailable, "route_fidelity_unavailable", "The current strict route contract could not be confirmed. Retry after the gateway refreshes its configuration.")
		}
	}
	return nil
}

func PolicyUnavailable(code, message string) *fault.Error {
	return &fault.Error{Status: http.StatusUnprocessableEntity, Type: "invalid_request_error", Code: code, Message: message}
}
