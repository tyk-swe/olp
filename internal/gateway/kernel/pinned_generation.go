package kernel

import (
	"context"
	"net/http"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) Enter(w http.ResponseWriter, r *http.Request, family wire.Format, resource bool, authenticate func(*http.Request) (keyauthority.Authority, *fault.Error)) (*Call, keyauthority.Authority, bool) {
	actor := "api_key"
	if resource {
		actor = "api_key_resource"
	}
	x := &Call{request: s.begin(w, r), family: family, actor: actor, mode: "unary"}
	authority, ok := s.enter(w, r, x, authenticate)
	return x, authority, ok
}

func (x *Call) ResourceCall() bool { return x.actor == "api_key_resource" }

func (x *Call) Mode() string { return x.mode }

func (x *Call) Streams() { x.mode = "streaming" }

func (x *Call) Dispatched() bool { return x.dispatched }

func (x *Call) Authority() keyauthority.Authority { return x.authority }

func (x *Call) RoutingControls(r *http.Request, authority keyauthority.Authority) *fault.Error {
	var e *fault.Error
	x.preferences, x.priority, e = routingControls(r, authority.Policy)
	return e
}

func (s *Server) ReserveGeneration(ctx context.Context, x *Call, authority keyauthority.Authority, bodyBytes int, route *routing.Route) *fault.Error {
	sized := int64(bodyBytes) / 4
	x.estimate, x.sizedInput = max(ResourceEstimate, sized), &sized
	var e *fault.Error
	x.lease, e = s.Deps.Admission.ReserveKey(ctx, x.AdmissionAuthority(authority), x.clientSurface(), x.estimate, time.Duration(route.OverallTimeout)*time.Millisecond, x.limitRoute())
	return e
}

func (s *Server) GatePinnedGeneration(ctx context.Context, x *Call, p *Pin, operation string, replyBound int64) *fault.Error {
	x.pinnedSlot = &p.Slot
	p.attempt.Price = s.Deps.Runtime.RoutingInputs().Price(string(p.Provider.Kind), p.Provider.ID, p.Provider.VendorID, p.Model, operation, s.Now())
	p.attempt.Discount = s.Deps.Runtime.RoutingInputs().Discount(string(p.Provider.Kind), p.Provider.ID, p.Provider.VendorID, s.Now())
	deadline, _ := ctx.Deadline()
	gate := s.gateAttempt(ctx, x, p.attempt, &p.Provider, &p.Slot, quota.GenerationDemand(x.estimate, replyBound, 1), deadline)
	if gate.verdict != gateAdmitted {
		return gateError(gate)
	}
	p.Hold = gate.hold
	x.attempts, x.budget = []routing.Attempt{p.attempt}, 1
	return nil
}

func (s *Server) SettleGeneration(ctx context.Context, x *Call, p *Pin) {
	if p != nil {
		s.settlePinnedHold(ctx, p.Hold)
	}
	s.settleAdmission(ctx, x)
}

func (s *Server) SessionCall(w http.ResponseWriter, r *http.Request, family wire.Format, mode string) *Call {
	return &Call{request: s.begin(w, r), family: family, actor: "api_key", mode: mode}
}

func (x *Call) Priority() string { return x.priority }

func (x *Call) EndUserDigest() string { return x.endUserDigest }

func (x *Call) AttributionValues() map[string]string { return x.attribution }

func (x *Call) ClientIP() string { return x.request.clientIP }

func (x *Call) SessionEstimate() { x.estimate = ResourceEstimate }

func (x *Call) LastAttemptFact() *events.AttemptFact {
	if len(x.facts) == 0 {
		return nil
	}
	return &x.facts[len(x.facts)-1]
}

func (s *Server) EnterBedrock(w http.ResponseWriter, r *http.Request, family wire.Format, mode string) (*Call, keyauthority.Authority, bool) {
	x := &Call{request: s.begin(w, r), family: family, actor: "api_key", mode: mode}
	authority, ok := s.enter(w, r, x, s.Gate.BedrockAuthenticate)
	return x, authority, ok
}

func (x *Call) SizedBody(bodyBytes int) {
	sized := int64(bodyBytes) / 4
	x.estimate, x.sizedInput = max(ResourceEstimate, sized), &sized
}

func (s *Server) ReleaseLease(ctx context.Context, x *Call) {
	quota.SettleKey(ctx, x.lease, false, nil, s.Log)
}

func (s *Server) SettleBedrock(ctx context.Context, x *Call, p *Pin) {
	actual := x.settledTokens()
	s.settlePinnedHold(ctx, p.Hold)
	s.recordCost(x)
	quota.SettleKey(ctx, x.lease, x.dispatched, actual, s.Log)
}

func (s *Server) EnterRetrieval(w http.ResponseWriter, r *http.Request) (*Call, keyauthority.Authority, bool) {
	x, authority, done := s.StateBegin(w, r, wire.File)
	x.operationOverride = "retrieval"
	x.mode = "unary"
	x.actor = "api_key_resource"
	x.retained = retainedOperation{use: RetainedNewWork, billing: retainedMeteredWork}
	return x, authority, done
}

func (s *Server) SelectRetrievalPin(ctx context.Context, x *Call, route *routing.Route, estimate int64, qualified func(*routing.Provider, string) bool) (*Pin, *fault.Error) {
	x.estimate = estimate
	return s.selectBoundPinSurface(ctx, x, route, "retrieval", "openai", "unary", resourceDemand(x), nil, qualified)
}

func (s *Server) ReserveRetrieval(ctx context.Context, x *Call, authority keyauthority.Authority, route *routing.Route) *fault.Error {
	if e := s.ReserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		return e
	}
	x.estimate = ResourceEstimate
	return nil
}

func (s *Server) GatePinnedRetrieval(ctx context.Context, x *Call, p *Pin) *fault.Error {
	x.pinnedSlot = &p.Slot
	deadline, _ := ctx.Deadline()
	gate := s.gateSlot(ctx, &p.Provider, &p.Slot, quota.CombinedDemand(ResourceEstimate), deadline, "")
	if gate.verdict != gateAdmitted {
		return gateError(gate)
	}
	p.Hold = gate.hold
	return nil
}

func (s *Server) SettleRetrieval(ctx context.Context, x *Call, p *Pin) {
	if p != nil {
		s.settlePinnedHold(ctx, p.Hold)
	}
	quota.SettleKey(ctx, x.lease, x.dispatched, x.settledTokens(), s.Log)
}

func ClassFailure(class string) *fault.Error {
	return (&AttemptFailure{class: class}).Fault()
}

func AttemptFailureFor(class string) *AttemptFailure {
	return &AttemptFailure{class: class}
}

func (x *Call) Sensitive() egress.Sensitive { return x.sensitive }

func (e *PinnedExchange) AcceptUnfinished() {
	fact := e.fact()
	fact.Class = ClassSuccess
	fact.ObserveUsage(fact.Usage, false)
	e.end()
}
