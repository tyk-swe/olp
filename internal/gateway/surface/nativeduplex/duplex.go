package nativeduplex

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/coder/websocket"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/routing"
	acceptance "github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

type turn struct {
	surface
	w          http.ResponseWriter
	r          *http.Request
	x          *kernel.Call
	authority  keyauthority.Authority
	token      string
	descriptor duplexcontract.Profile
	family     string
	end        *turnEnd
}

type turnEnd struct {
	pin    *kernel.Pin
	status int
	out    *kernel.Outcome
}

type closeError struct {
	fault  *fault.Error
	code   websocket.StatusCode
	reason string
}

func policyClose(e *fault.Error, reason string) *closeError {
	return &closeError{fault: e, code: websocket.StatusPolicyViolation, reason: reason}
}

func (t turn) fail(e *fault.Error) {
	t.x.Failure, t.end.status = e, e.Status
	fault.WriteSurfaceError(t.w, e, t.family)
}

func (t turn) failClose(client *websocket.Conn, ce *closeError) {
	client.Close(ce.code, ce.reason)
	t.x.Failure, t.end.status = ce.fault, ce.fault.Status
}

// Native duplex sessions are connection scoped. The gateway mints no session
// identifier and exposes no follow-up call that could address one, so owner
// scoping is the authenticated connection itself, opened under one key
// against one published route. Provider session identifiers inside relayed
// frames stay opaque payload and mean nothing at the gateway boundary.
func (s surface) duplex(w http.ResponseWriter, r *http.Request) {
	dialect, routeSlug := r.PathValue("dialect"), r.PathValue("route")
	x := s.kernel.SessionCall(w, r, wire.NativeDuplex, "realtime")
	end := &turnEnd{status: http.StatusInternalServerError}
	defer func() { s.kernel.Finish(x, end.out, end.status) }()
	fail := func(e *fault.Error) {
		x.Failure, end.status = e, e.Status
		fault.WriteSurfaceError(w, e, fault.RequestSurface(r))
	}
	descriptor, known := duplexcontract.Lookup(dialect)
	if !known {
		fail(fault.NotFoundError("not_found", "Unknown native duplex dialect."))
		return
	}
	if !s.kernel.Gate.Admit(r.Context()) {
		fail(fault.Overloaded)
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	authority, token, e := s.admitDuplexCaller(x, r)
	if e != nil {
		fail(e)
		return
	}
	t := turn{surface: s, w: w, r: r, x: x, authority: authority, token: token, descriptor: descriptor, family: fault.RequestSurface(r), end: end}
	route, e := t.routeFor(routeSlug)
	if e != nil {
		t.fail(e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kernel.SessionExposureWindow)
	defer cancel()
	client, e := t.acceptClient()
	if e != nil {
		t.fail(e)
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	initial, limit, ce := t.readSetup(ctx, client, route)
	if ce != nil {
		t.failClose(client, ce)
		return
	}
	if ce := t.checkExposure(ctx, client); ce != nil {
		t.failClose(client, ce)
		return
	}
	x.SessionEstimate()
	if e := s.kernel.ReserveState(ctx, x, authority, kernel.SessionExposureWindow); e != nil {
		t.failClose(client, policyClose(e, "admission refused"))
		return
	}
	defer func() { s.kernel.SettleSession(r.Context(), x, t.end.pin) }()
	pin, template, ce := t.pinAndDebit(ctx, route)
	if ce != nil {
		t.failClose(client, ce)
		return
	}
	upstream, observer, ce := t.dispatchSetup(ctx, client, pin, template, initial, limit)
	if upstream != nil {
		defer upstream.Close(websocket.StatusNormalClosure, "")
	}
	if ce != nil {
		t.failClose(client, ce)
		return
	}
	usageRecord, complete, relayErr := s.relayNativeDuplex(ctx, x, pin, client, upstream, token, observer, template.Transport(), int(limit))
	t.finishRelay(usageRecord, complete, relayErr)
}

func (s surface) admitDuplexCaller(x *kernel.Call, r *http.Request) (keyauthority.Authority, string, *fault.Error) {
	if !s.kernel.BrowserOriginAllowed(r) {
		return keyauthority.Authority{}, "", fault.PermissionError("origin_forbidden", "This origin is not allowed to open duplex sessions.")
	}
	if !kernel.RealtimeUpgrade(r) {
		return keyauthority.Authority{}, "", fault.InvalidRequest("websocket_upgrade_required", "Native duplex sessions require a WebSocket upgrade.", nil)
	}
	token := ingress.BearerToken(r)
	if len(r.Header.Values("Authorization")) != 1 || token == "" {
		return keyauthority.Authority{}, "", fault.AuthenticationError("invalid_api_key", "Provide one API key.")
	}
	authority, e := s.kernel.Gate.AuthenticateRequest(r, token, "inference")
	if e != nil {
		return keyauthority.Authority{}, "", e
	}
	s.kernel.BindAuthority(x, authority)
	if e := x.NoteAttribution(r, authority); e != nil {
		return keyauthority.Authority{}, "", e
	}
	return authority, token, nil
}

func (t turn) routeFor(slug string) (*routing.Route, *fault.Error) {
	route, ok := t.x.Snapshot().FindRoute(slug)
	if !ok || !t.x.Authority().Allows("inference", route.Slug, route.ProjectID, t.kernel.Now()) || !slices.Contains(route.Operations, "realtime") {
		return nil, fault.ModelNotFound(slug)
	}
	t.x.Route = &route
	if e := t.x.GuardrailSurfaceGate(&route); e != nil {
		return nil, e
	}
	if e := t.x.RoutingControls(t.r, t.x.Authority()); e != nil {
		return nil, e
	}
	if e := kernel.UnboundedWorkLimits(t.x.AdmissionAuthority(t.x.Authority()), nil, nil, &route); e != nil {
		return nil, e
	}
	return &route, nil
}

func (t turn) readSetup(ctx context.Context, client *websocket.Conn, route *routing.Route) ([]byte, int64, *closeError) {
	limit := t.kernel.Config.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	client.SetReadLimit(limit)
	setupCtx, stopSetup := context.WithTimeout(ctx, kernel.RequestBodyTimeout)
	messageType, initial, err := client.Read(setupCtx)
	stopSetup()
	if err != nil || messageType != websocket.MessageText {
		return nil, limit, &closeError{fault: fault.InvalidRequest("invalid_duplex_setup", "A native duplex session requires a setup frame first.", nil), code: websocket.StatusPolicyViolation, reason: "setup required"}
	}
	setup, inspectErr := duplexcontract.InspectProfileSetup(t.descriptor, initial, int(limit))
	if inspectErr == nil && setup.Model != "" && setup.Model != route.Slug {
		inspectErr = errDuplexClientProtocol
	}
	if inspectErr == nil {
		inspectErr = duplexcontract.AdmitProfileSetup(t.descriptor, setup)
	}
	if inspectErr != nil {
		return nil, limit, &closeError{fault: fault.InvalidRequest("invalid_duplex_setup", "The native duplex setup is not admitted by the compiled profile.", nil), code: websocket.StatusPolicyViolation, reason: "setup refused"}
	}
	if route.MaxBodyBytes != nil && int64(len(initial)) > *route.MaxBodyBytes {
		return nil, limit, &closeError{fault: ingress.BodyTooLarge(), code: websocket.StatusMessageTooBig, reason: "request body limit exceeded"}
	}
	client.SetReadLimit(bodylimit.Lower(limit, route.MaxBodyBytes))
	return initial, limit, nil
}

func (t turn) checkExposure(ctx context.Context, _ *websocket.Conn) *closeError {
	held, exposureErr := t.kernel.SessionExposureHeld(ctx, t.x.Authority().ID)
	if exposureErr != nil {
		return &closeError{fault: exposureErr, code: websocket.StatusPolicyViolation, reason: "exposure check unavailable"}
	}
	if held {
		return &closeError{fault: fault.ServerError(http.StatusConflict, "session_exposure_unresolved", "An earlier session for this key is still billing-uncertain; new sessions are refused until that exposure resolves."), code: websocket.StatusPolicyViolation, reason: "unresolved session exposure"}
	}
	return nil
}

func (t turn) pinAndDebit(ctx context.Context, route *routing.Route) (*kernel.Pin, *duplexcontract.Template, *closeError) {
	p, e := t.kernel.SelectPinSurface(ctx, t.x, route, "realtime", "native", "realtime", func(provider *routing.Provider, model string) bool {
		return provider.ProfileID == t.descriptor.ProfileID && provider.Connector().Supports("realtime", "native", "realtime") && provider.Supports(model, "realtime", "native", "realtime")
	})
	t.end.pin = p
	if e != nil {
		return p, nil, policyClose(e, "provider unavailable")
	}
	template, compiled := t.x.Snapshot().DuplexTemplate(route.Slug, p.Target.ID)
	if !compiled || template.Dialect() != t.descriptor.Dialect {
		return p, nil, policyClose(fault.ServerError(http.StatusBadGateway, "upstream_error", "The selected target has no compiled native duplex contract."), "profile unavailable")
	}
	if e := t.kernel.UnboundedPinLimits(t.x.AdmissionAuthority(t.x.Authority()), p, route); e != nil {
		return p, nil, policyClose(e, "unbounded work cannot enforce budgets")
	}
	if e := t.kernel.ReservePinSessionDebit(ctx, t.x, t.x.Authority(), p, nil); e != nil {
		return p, nil, policyClose(e, "admission refused")
	}
	if e := t.kernel.ReserveSessionCost(ctx, t.x, t.x.Authority(), p, kernel.SessionExposureWindow); e != nil {
		return p, nil, policyClose(e, "admission refused")
	}
	if e := t.kernel.MarkSessionDispatched(ctx, t.x); e != nil {
		return p, nil, policyClose(e, "admission refused")
	}
	return p, template, nil
}

func (t turn) dispatchSetup(ctx context.Context, client *websocket.Conn, p *kernel.Pin, template *duplexcontract.Template, initial []byte, limit int64) (kernel.DuplexPeer, *duplexcontract.Observer, *closeError) {
	upstream, e := t.surface.duplexDial(ctx, t.x, p, template)
	if e != nil {
		return nil, nil, &closeError{fault: e, code: websocket.StatusInternalError, reason: "provider unavailable"}
	}
	boundModel, modelErr := p.Provider.Connector().DuplexBoundModel(p.Model)
	bound, bindErr := template.BindSetup(initial, boundModel)
	if modelErr != nil {
		bindErr = modelErr
	}
	invalidSetup := &closeError{fault: fault.InvalidRequest("invalid_duplex_setup", "The native duplex setup could not be bound.", nil), code: websocket.StatusPolicyViolation, reason: "invalid setup"}
	if bindErr != nil || int64(len(bound)) > limit {
		return upstream, nil, invalidSetup
	}
	observer := template.Observer(int(limit))
	if err := template.ObserveSetupFrame(observer, bound); err != nil {
		return upstream, nil, invalidSetup
	}
	t.x.MarkDispatched()
	if err := kernel.WriteDuplexFrame(ctx, upstream, websocket.MessageText, bound); err != nil {
		markFactUncertain(t.x, kernel.ClassAmbiguous)
		return upstream, nil, &closeError{fault: fault.ServerError(http.StatusBadGateway, "upstream_outcome_unknown", "The duplex setup outcome is unknown."), code: websocket.StatusInternalError, reason: "setup uncertain"}
	}
	if template.SetupAck() {
		if ackErr := awaitSetupAck(ctx, upstream, client, observer, p.Timeout()); ackErr != nil {
			if errors.Is(ackErr, errDuplexProtocol) {
				markFactUncertain(t.x, kernel.ClassProtocol)
				return upstream, nil, &closeError{fault: fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The duplex provider sent an invalid setup acknowledgement."), code: websocket.StatusInternalError, reason: "setup uncertain"}
			}
			markFactUncertain(t.x, kernel.ClassAmbiguous)
			return upstream, nil, &closeError{fault: fault.ServerError(http.StatusBadGateway, "upstream_outcome_unknown", "The duplex provider did not acknowledge setup."), code: websocket.StatusInternalError, reason: "setup uncertain"}
		}
	}
	t.x.Delivered(t.kernel.Now())
	if fact := t.x.LastAttemptFact(); fact != nil && fact.Interaction != nil {
		fact.Interaction.UpstreamState = acceptance.Accepted
		fact.Interaction.ClientState = usage.ClientPartial
	}
	return upstream, observer, nil
}

func (t turn) acceptClient() (*websocket.Conn, *fault.Error) {
	client, err := websocket.Accept(t.w, t.r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, fault.ServerError(http.StatusBadRequest, "websocket_upgrade_failed", "The duplex WebSocket upgrade failed.")
	}
	return client, nil
}

func (t turn) finishRelay(usageRecord *wire.Usage, usageComplete bool, relayErr error) {
	x := t.x
	class := kernel.ClassProtocol
	cancelled := false
	if x.Strict() && relayErr != nil {
		class = kernel.ClassAmbiguous
		switch {
		case errors.Is(relayErr, errDuplexProviderError):
			class = kernel.ClassUpstreamServer
		case errors.Is(relayErr, errDuplexClientProtocol), errors.Is(relayErr, errDuplexProtocol):
			class = kernel.ClassProtocol
		case errors.Is(relayErr, errDuplexAuthority):
			class = kernel.ClassCredential
		case errors.Is(relayErr, errDuplexClientClosed), errors.Is(relayErr, context.Canceled):
			class, cancelled = kernel.ClassCancelled, true
		case errors.Is(relayErr, context.DeadlineExceeded):
			class = kernel.ClassTimeout
		}
	}
	if fact := x.LastAttemptFact(); fact != nil {
		fact.Usage = usageRecord
		if usageRecord != nil {
			fact.UsageObserved, fact.UsageComplete, fact.BillingUncertain = true, usageComplete && relayErr == nil, !usageComplete || relayErr != nil
		} else {
			fact.UsageComplete, fact.BillingUncertain = false, true
		}
		if relayErr != nil {
			fact.Class = class
			if fact.Interaction != nil {
				if errors.Is(relayErr, errDuplexProviderError) {
					fact.Interaction.UpstreamState = acceptance.Terminal
				} else {
					fact.Interaction.UpstreamState = acceptance.Unknown
				}
			}
		} else if fact.Interaction != nil {
			fact.Interaction.UpstreamState, fact.Interaction.ClientState = acceptance.Terminal, usage.ClientTerminal
		}
	}
	if relayErr == nil {
		t.end.status, t.end.out = http.StatusOK, &kernel.Outcome{Committed: true}
		if x.Strict() {
			t.end.status = http.StatusSwitchingProtocols
		}
		return
	}
	x.Failure, t.end.status = fault.ServerError(http.StatusBadGateway, "duplex_incomplete", "The duplex session ended without a complete transport contract."), http.StatusBadGateway
	if !x.Strict() {
		return
	}
	switch {
	case errors.Is(relayErr, errDuplexProviderError):
		x.Failure = fault.ServerError(http.StatusBadGateway, "duplex_provider_error", "The duplex provider reported an error.")
	case errors.Is(relayErr, errDuplexProtocol):
		x.Failure = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The duplex provider sent an invalid frame.")
	case errors.Is(relayErr, errDuplexClientProtocol):
		x.Failure = fault.InvalidRequest("invalid_duplex_frame", "The duplex client sent an invalid frame.", nil)
	case errors.Is(relayErr, errDuplexAuthority):
		x.Failure = fault.PermissionError("key_revoked", "The duplex session no longer has route authority.")
	case class == kernel.ClassCancelled:
		x.Failure = kernel.ClassFailure(kernel.ClassCancelled)
	case class == kernel.ClassTimeout:
		x.Failure = kernel.ClassFailure(kernel.ClassTimeout)
	}
	t.end.status = http.StatusSwitchingProtocols
	t.end.out = &kernel.Outcome{Err: x.Failure, Committed: true, Cancelled: cancelled}
}

func awaitSetupAck(ctx context.Context, upstream, client kernel.DuplexPeer, observer *duplexcontract.Observer, timeout time.Duration) error {
	ackCtx, stopAck := context.WithTimeout(ctx, timeout)
	kind, ack, err := upstream.Read(ackCtx)
	stopAck()
	if err != nil {
		return err
	}
	if kind != websocket.MessageText {
		return errDuplexProtocol
	}
	frame, err := observer.ServerFrame(ack)
	if err != nil || !frame.SetupComplete {
		return errDuplexProtocol
	}
	return kernel.WriteDuplexFrame(ctx, client, kind, ack)
}

func markFactUncertain(x *kernel.Call, class string) {
	fact := x.LastAttemptFact()
	if fact == nil {
		return
	}
	fact.Class, fact.BillingUncertain = class, true
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = acceptance.Unknown
	}
}
