package gateway

import "github.com/tyk-swe/olp/internal/usage"

func (s *Server) routingInputs() *usage.RoutingInputs {
	if source, ok := s.Runtime.(interface{ RoutingInputs() *usage.RoutingInputs }); ok {
		return source.RoutingInputs()
	}
	return nil
}
