package kernel

import (
	"context"
	"net/http"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
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
	x.estimate, x.sizedInput = max(resourceEstimate, sized), &sized
	var e *fault.Error
	x.lease, e = s.Deps.Admission.ReserveKey(ctx, x.AdmissionAuthority(authority), x.clientSurface(), x.estimate, time.Duration(route.OverallTimeout)*time.Millisecond, x.limitRoute())
	return e
}

func (s *Server) GatePinnedGeneration(ctx context.Context, x *Call, p *Pin, operation string) *fault.Error {
	x.pinnedSlot = &p.Slot
	p.attempt.Price = s.Deps.Runtime.RoutingInputs().Price(string(p.Provider.Kind), p.Provider.ID, p.Provider.VendorID, p.Model, operation, s.Now())
	p.attempt.Discount = s.Deps.Runtime.RoutingInputs().Discount(string(p.Provider.Kind), p.Provider.ID, p.Provider.VendorID, s.Now())
	deadline, _ := ctx.Deadline()
	gate := s.gateAttempt(ctx, x, p.attempt, &p.Provider, &p.Slot, x.estimate, deadline)
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

func (s *Server) ReserveRetrieval(ctx context.Context, x *Call, authority keyauthority.Authority, route *routing.Route) *fault.Error {
	if e := s.reserveState(ctx, x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		return e
	}
	x.estimate = resourceEstimate
	return nil
}

func (s *Server) GatePinnedRetrieval(ctx context.Context, x *Call, p *Pin) *fault.Error {
	x.pinnedSlot = &p.Slot
	deadline, _ := ctx.Deadline()
	gate := s.gateSlot(ctx, &p.Provider, &p.Slot, resourceEstimate, deadline, "")
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

func (x *Call) Sensitive() egress.Sensitive { return x.sensitive }

func (e *PinnedExchange) AcceptUnfinished() {
	fact := e.fact()
	fact.Class = ClassSuccess
	fact.ObserveUsage(fact.Usage, false)
	e.end()
}
