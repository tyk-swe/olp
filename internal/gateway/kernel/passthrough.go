package kernel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s *Server) EnterPassthrough(w http.ResponseWriter, r *http.Request) (*Call, keyauthority.Authority, bool) {
	x := &Call{request: s.begin(w, r), family: wire.Passthrough, actor: "api_key", mode: "unary"}
	if !s.Gate.Admit(r.Context()) {
		s.RefusePassthrough(x, w, fault.Overloaded)
		return nil, keyauthority.Authority{}, false
	}
	authority, e := s.authorizePassthrough(r, x)
	if e != nil {
		s.RefusePassthrough(x, w, e)
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

func (s *Server) RefusePassthrough(x *Call, w http.ResponseWriter, e *fault.Error) {
	s.RefuseOn(x, w, e, "openai")
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

func (s *Server) DispatchPassthrough(ctx context.Context, x *Call, p Pin, r *http.Request, upstream string, body []byte) (*http.Response, *fault.Error) {
	deadline, _ := ctx.Deadline()
	if e := s.reserveSessionCost(ctx, x, x.authority, p.attempt, time.Until(deadline)); e != nil {
		return nil, e
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, upstream, bytes.NewReader(body))
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider request could not be built.")
	}
	forwardPassthroughHeaders(req.Header, r.Header, x.Route.Passthrough.Headers, len(body) > 0)
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

func forwardPassthroughHeaders(dst, src http.Header, allowed []string, hasBody bool) {
	headers := codewire.ForwardHeaders(src, true)
	for _, name := range allowed {
		for _, value := range headers.Values(name) {
			dst.Add(name, value)
		}
	}
	if ct := headers.Get("Content-Type"); ct != "" && hasBody {
		dst.Set("Content-Type", ct)
	}
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

func (s *Server) ReceivePassthrough(ctx context.Context, x *Call, method string, resp *http.Response) (io.Reader, int64, *fault.Error) {
	fact := x.LastAttemptFact()
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		s.closePassthroughFact(fact, ClassProtocol, true)
		return nil, 0, passthroughRedirectRefused()
	}
	limit := s.passthroughResponseLimit()
	if method != http.MethodHead && resp.ContentLength > limit {
		s.closePassthroughFact(fact, ClassProtocol, true)
		return nil, 0, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response exceeded the gateway limit.")
	}
	body, e := passthroughResponseBody(resp, x.sensitive, limit)
	if e != nil {
		s.closePassthroughFact(fact, timeoutOr(ctx, ClassProtocol), true)
		return nil, 0, e
	}
	return body, limit, nil
}

func (s *Server) passthroughResponseLimit() int64 {
	if s.Config.MaxResponseBytes <= 0 {
		return 1 << 20
	}
	return s.Config.MaxResponseBytes
}

func passthroughResponseBody(response *http.Response, sensitive egress.Sensitive, limit int64) (io.Reader, *fault.Error) {
	if response.StatusCode < 400 || response.Request != nil && response.Request.Method == http.MethodHead {
		return response.Body, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider error response could not be read.")
	}
	decoded, err := codewire.Decode(raw, response.Header.Get("Content-Encoding"), limit)
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider error response could not be decoded within the gateway limit.")
	}
	redacted := sensitive.Redact(string(decoded))
	if int64(len(redacted)) > limit {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider error response exceeded the gateway limit.")
	}
	return strings.NewReader(redacted), nil
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
