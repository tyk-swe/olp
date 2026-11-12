package gateway

import (
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/routing"
)

func (x *call) checkBody(route *routing.Route) *fault.Error {
	if route == nil || x.request.body == nil {
		return nil
	}
	// Retained resources may return a historical serving route; ingress policy
	// comes from the request's pinned release.
	if x.request.release != nil && x.request.release.Snapshot() != nil {
		if current, ok := x.request.release.Snapshot().FindRoute(route.Slug); ok {
			route = &current
		}
	}
	if route.MaxBodyBytes != nil && x.request.body.Size() > *route.MaxBodyBytes {
		return ingress.BodyTooLarge()
	}
	return nil
}
