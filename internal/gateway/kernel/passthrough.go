package kernel

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) EnterPassthrough(w http.ResponseWriter, r *http.Request) (*Call, keyauthority.Authority, bool) {
	x := newCall(s.begin(w, r), wire.Passthrough, callIdentity{actor: "api_key"}, callLineage{}, callWork{mode: "unary"})
	if !s.Gate.Admit(r.Context()) {
		s.RefuseOn(x, w, fault.Overloaded, "openai")
		return nil, keyauthority.Authority{}, false
	}
	authority, e := s.authorizePassthrough(r, x)
	if e != nil {
		s.RefuseOn(x, w, e, "openai")
		s.Gate.Release(r.Context())
		return nil, keyauthority.Authority{}, false
	}
	return x, authority, true
}

func (s *Server) authorizePassthrough(r *http.Request, x *Call) (keyauthority.Authority, *fault.Error) {
	authority, e := s.Gate.Authenticate(r, "inference")
	if e != nil {
		return authority, e
	}
	s.BindAuthority(x, authority)
	if e := x.RoutingControls(r, authority); e != nil {
		return authority, e
	}
	return authority, x.NoteAttribution(r, authority)
}

func (s *Server) SelectPassthrough(ctx context.Context, x *Call) (Pin, *fault.Error) {
	target := x.Route.Targets[0]
	provider, ok := x.Snapshot().FindProvider(target.ProviderID)
	if !ok || !provider.Enabled {
		return Pin{}, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "The bound connection is unavailable.")
	}
	options := s.selectionOptions(ctx, x)
	options.CheckSlots = false
	plan, planErr := routing.Select(x.Snapshot(), x.Route.Slug, routing.PassthroughOperation, "native", "unary", x.affinity, options)
	if planErr != nil {
		return Pin{}, selectionError(planErr, x.Route.Slug)
	}
	i := slices.IndexFunc(plan.Attempts, func(planned routing.Attempt) bool { return planned.TargetID == target.ID })
	if i < 0 {
		return Pin{}, selectionError(&routing.SelectionError{Code: routing.NoEligibleTargets}, x.Route.Slug)
	}
	return Pin{Target: target, Provider: provider, attempt: plan.Attempts[i]}, nil
}

func (s *Server) GatePassthrough(ctx context.Context, x *Call, p Pin, deadline time.Time) (Pin, *fault.Error) {
	demand := quota.CombinedDemand(max(ResourceEstimate, x.estimate))
	unmeterable := false
	for _, candidate := range s.slots(x, p.attempt, &p.Provider) {
		gate := s.gateAttempt(ctx, x, p.attempt, &p.Provider, &candidate, demand, deadline)
		switch gate.verdict {
		case gateAdmitted:
			return Pin{Target: p.Target, Provider: p.Provider, attempt: p.attempt, Slot: candidate, Hold: gate.hold}, nil
		case gateRejected:
			x.facts = append(x.facts, s.rejectedFact(x, p.attempt, candidate, 1, gate.rejection))
			return Pin{}, gate.rejection.Fault()
		case gateExpired:
			return Pin{}, ClassFailure(ClassTimeout)
		case gateUnmeterable:
			unmeterable = true
		case gateDenied:
			return unslottedPassthrough(x, p, unmeterable)
		}
	}
	return unslottedPassthrough(x, p, unmeterable)
}

func unslottedPassthrough(x *Call, p Pin, unmeterable bool) (Pin, *fault.Error) {
	if unmeterable {
		return Pin{}, quota.LimitsUnavailable()
	}
	if len(p.Provider.Slots) > 0 {
		return Pin{}, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No credential slot is currently able to serve `"+x.Route.Slug+"`.")
	}
	return p, nil
}

func (s *Server) SettlePassthrough(ctx context.Context, x *Call, p *Pin) {
	if p != nil {
		if x.dispatched && p.Hold != nil {
			x.spendCaps(p.Hold.budgets, p.attempt)
		}
		s.SettleSessionHold(ctx, x, p.Hold, quota.SettledAttemptTokens(x.LastAttemptFact(), max(ResourceEstimate, x.estimate)))
	}
	s.SettleCaps(ctx, x)
}

func (s *Server) DispatchPassthrough(ctx context.Context, x *Call, p Pin, method, upstream string, headers http.Header, body []byte) (*http.Response, *fault.Error) {
	deadline, _ := ctx.Deadline()
	if e := s.reserveSessionCost(ctx, x, x.authority, p.attempt, time.Until(deadline)); e != nil {
		return nil, e
	}
	req, err := http.NewRequestWithContext(ctx, method, upstream, bytes.NewReader(body))
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider request could not be built.")
	}
	maps.Copy(req.Header, headers)
	fact := s.recordPassthroughFact(x, p)
	if err := s.applySlotCredential(ctx, x, req, p.Provider.Connector(), p.Slot, body, &p.Provider, p.Target.ProviderModel); err != nil {
		s.closePassthroughFact(fact, ClassCredential, false)
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The bound credential could not be applied.")
	}
	client, err := s.providerClient(ctx, x.request.release, &p.Provider, p.Slot)
	if err != nil {
		s.closePassthroughFact(fact, ClassCredential, false)
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The bound connection could not be reached.")
	}
	x.dispatched = true
	resp, err := client.Do(req)
	if err != nil {
		return nil, s.passthroughTransportFailure(ctx, fact, resp, err)
	}
	received := s.Now().Sub(fact.StartedAt)
	fact.FirstByte, fact.Status = &received, resp.StatusCode
	return resp, nil
}

func (s *Server) FailPassthrough(x *Call, class string) {
	s.closePassthroughFact(x.LastAttemptFact(), class, true)
}

func (s *Server) recordPassthroughFact(x *Call, p Pin) *events.AttemptFact {
	fact := s.newFact(x, p.attempt, p.Slot, 1, &p.Provider)
	fact.Mode = "unary"
	fact.PricingPinned = p.attempt.Price != nil
	if p.Hold != nil {
		fact.Budgets = p.Hold.budgets
	}
	x.facts = append(x.facts, fact)
	return &x.facts[len(x.facts)-1]
}

func (s *Server) closePassthroughFact(fact *events.AttemptFact, class string, evidence bool) {
	fact.Class, fact.Duration = class, s.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(evidence)
}

func timeoutOr(ctx context.Context, class string) string {
	if ctx.Err() != nil {
		return ClassTimeout
	}
	return class
}

func (s *Server) passthroughTransportFailure(ctx context.Context, fact *events.AttemptFact, resp *http.Response, err error) *fault.Error {
	if resp != nil && resp.StatusCode >= 300 && resp.StatusCode < 400 || errors.Is(err, egress.ErrRedirect) || strings.Contains(err.Error(), egress.ErrRedirect.Error()) {
		if resp != nil {
			fact.Status = resp.StatusCode
		}
		s.closePassthroughFact(fact, ClassProtocol, true)
		return passthroughRedirectRefused()
	}
	s.closePassthroughFact(fact, timeoutOr(ctx, ClassConnect), true)
	return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
}

func passthroughRedirectRefused() *fault.Error {
	return fault.ServerError(http.StatusBadGateway, "passthrough_redirect_refused", "The provider redirect was refused.")
}

func (s *Server) FinishPassthrough(ctx context.Context, x *Call, status int, readErr, copyErr error) {
	providerSuccess := readErr == nil && (status >= 200 && status < 300 || status == http.StatusNotModified)
	s.classifyPassthrough(ctx, x.LastAttemptFact(), status, readErr, providerSuccess)
	s.Finish(x, &Outcome{Committed: providerSuccess && copyErr == nil, Err: passthroughOutcomeError(status, readErr, copyErr, providerSuccess)}, status)
}

func (s *Server) classifyPassthrough(ctx context.Context, fact *events.AttemptFact, status int, readErr error, providerSuccess bool) {
	switch {
	case readErr != nil:
		s.closePassthroughFact(fact, timeoutOr(ctx, ClassProtocol), true)
	case providerSuccess:
		fact.Duration = s.Now().Sub(fact.StartedAt)
		fact.Class = ClassSuccess
		fact.ObserveUsage(&wire.Usage{}, true)
		fact.TokenlessUsage = true
		fact.RecordEvidence(false)
	case status >= 500:
		s.closePassthroughFact(fact, ClassUpstreamServer, true)
	case status >= 400:
		s.closePassthroughFact(fact, classUpstreamClient, true)
	default:
		s.closePassthroughFact(fact, ClassProtocol, true)
	}
}

func passthroughOutcomeError(status int, readErr, copyErr error, providerSuccess bool) *fault.Error {
	if readErr != nil || copyErr != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be delivered.")
	}
	if !providerSuccess {
		return UpstreamError(&AttemptFailure{Status: status})
	}
	return nil
}
