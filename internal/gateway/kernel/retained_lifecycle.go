package kernel

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

func (s *Server) RetainedSubmissionDigest(endpoint retainedapi.Endpoint, slug string, body []byte) ([]byte, *fault.Error) {
	digester := s.Deps.Digester
	if digester == nil {
		return nil, fault.ServerError(503, "authority_unavailable", "Resource submission identity is unavailable.")
	}
	canonical, err := CanonicalBatchJSON(body)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", "A valid JSON object is required.", nil)
	}
	return digester.Digest(secrets.RetainedSubmissionDigest, endpoint.Operation+"\x00"+endpoint.Kind+"\x00"+slug+"\x00"+string(canonical)), nil
}

func RetainedAffinity(resource *resources.Resource, p *Pin, route *routing.Route) bool {
	return resource != nil && resource.RouteSlug == route.Slug && resource.RouteRevisionID == route.RevisionID && resource.ProviderID == p.Provider.ID && resource.ProviderRevisionID == p.Provider.RevisionID && resource.TargetID == p.Target.ID && resource.SlotID == p.Slot.ID && sameCredential(resource.CredentialID, p.Slot.CredentialID)
}
func sameCredential(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (s *Server) RetainedResponseLimit() int64 {
	if s.Config.MaxResponseBytes > 0 && s.Config.MaxResponseBytes < RetainedBodyLimit {
		return s.Config.MaxResponseBytes
	}
	return RetainedBodyLimit
}

// Provider errors may echo native resource identifiers. Preserve the stable
// HTTP status while keeping provider identities outside public errors.
func RetainedPublicError(err *fault.Error, status int) *fault.Error {
	out := *err
	if status >= http.StatusBadRequest && status <= 599 {
		out.Status = status
	}
	out.Message = "The provider could not complete this resource operation."
	out.Param = nil
	out.Code = "provider_resource_error"
	out.Type = "provider_error"
	return &out
}

const RetainedBodyLimit = 4 << 20

func (s *Server) GateRetainedSlot(ctx context.Context, x *Call, p *Pin, route *routing.Route) *fault.Error {
	if p.Hold != nil {
		return nil
	}
	// Bound work crosses the same spend-cap boundary as a fresh attempt:
	// gateSlot alone would reserve token quota but never the route,
	// provider, or credential-slot spend caps pinned on the attempt.
	// Spend caps are evaluated against the attempt's route, which some
	// collection handlers carry only as this argument.
	if x.Route == nil {
		x.Route = route
	}
	provider, slot := &p.Provider, &p.Slot
	if x.freeRetainedControl() {
		copyProvider := p.Provider
		copySlot := p.Slot
		copySlot.TokensPerMinute = nil
		copySlot.InputTokensPerMinute = nil
		copySlot.OutputTokensPerMinute = nil
		if p.Provider.Limits != nil {
			copyLimits := *p.Provider.Limits
			copyLimits.TokensPerMinute = nil
			copyLimits.InputTokensPerMinute = nil
			copyLimits.OutputTokensPerMinute = nil
			copyProvider.Limits = &copyLimits
		}
		provider, slot = &copyProvider, &copySlot
	}
	gate := s.gateAttempt(ctx, x, p.attempt, provider, slot, quota.CombinedDemand(max(ResourceEstimate, x.estimate)), s.Now().Add(time.Duration(route.OverallTimeout)*time.Millisecond))
	if gate.verdict != gateAdmitted {
		return gateError(gate)
	}
	p.Hold = gate.hold
	return nil
}

// Retained controls require durable credential identity and known, unpriced
// execution because this contract has no model usage observations.
func (s *Server) RetainedControlPolicy(x *Call, p *Pin, route *routing.Route) *fault.Error {
	if p.Provider.CredentialSource == "caller" {
		return PolicyUnavailable("unsupported_provider_credentials", "Agent resources require managed provider credentials.")
	}
	if x.routingInputs == nil && s.Deps.Runtime != nil {
		x.routingInputs = s.Deps.Runtime.RoutingInputs()
	}
	if x.routingInputs == nil || x.request.startedAt.Sub(x.routingInputs.RefreshedAt) > routinginputs.PerformanceMaxAge {
		return fault.ServerError(http.StatusServiceUnavailable, "pricing_unavailable", "Current resource pricing is unavailable; retry shortly.")
	}
	if p.attempt.Price == nil {
		p.attempt.Price = x.routingInputs.Price(string(p.Provider.Kind), p.Provider.ID, p.Provider.VendorID, p.Model, x.OperationName(), x.request.startedAt)
	}
	if p.attempt.Price != nil || x.attemptCardRevisionForRoute(p.Provider.ID, route, x.request.startedAt) != nil {
		return PolicyUnavailable("unsupported_pricing", "Agent resource control does not support priced execution. Use an unpriced resource route.")
	}
	return nil
}

func RetainedUnsettled(state string) bool {
	return state == "submitting" || state == "submission_unknown" || state == "not_accepted"
}
