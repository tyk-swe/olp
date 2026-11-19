package kernel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/platform/panics"
	acceptance "github.com/tyk-swe/olp/internal/upstream"

	"github.com/coder/websocket"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

const nativeDuplexSession = time.Hour

var (
	errDuplexProviderError  = errors.New("native duplex provider reported an error")
	errDuplexProtocol       = errors.New("native duplex provider protocol violation")
	errDuplexClientProtocol = errors.New("native duplex client protocol violation")
	errDuplexAuthority      = errors.New("native duplex authority revoked")
	errDuplexClientClosed   = errors.New("native duplex client closed with work in flight")
	errDuplexIncomplete     = errors.New("native duplex provider closed with work in flight")
)

// Native duplex sessions are connection scoped. The gateway mints no session
// identifier and exposes no follow-up call that could address one, so owner
// scoping is the authenticated connection itself, opened under one key
// against one published route. Provider session identifiers inside relayed
// frames stay opaque payload and mean nothing at the gateway boundary.
func (s *Server) nativeDuplex(w http.ResponseWriter, r *http.Request) {
	dialect, routeSlug := r.PathValue("dialect"), r.PathValue("route")
	x := &Call{request: s.begin(w, r), family: wire.NativeDuplex, actor: "api_key", mode: "realtime"}
	status := http.StatusInternalServerError
	var out *Outcome
	defer func() { s.Finish(x, out, status) }()
	surface := fault.RequestSurface(r)
	descriptor, known := duplexcontract.Lookup(dialect)
	if !known {
		e := fault.NotFoundError("not_found", "Unknown native duplex dialect.")
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	if !s.Gate.Admit(r.Context()) {
		x.Failure, status = fault.Overloaded, fault.Overloaded.Status
		fault.WriteSurfaceError(w, fault.Overloaded, surface)
		return
	}
	defer s.Gate.Release(r.Context())
	if !s.browserOriginAllowed(r) {
		e := fault.PermissionError("origin_forbidden", "This origin is not allowed to open duplex sessions.")
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	if !realtimeUpgrade(r) {
		e := fault.InvalidRequest("websocket_upgrade_required", "Native duplex sessions require a WebSocket upgrade.", nil)
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	token := ingress.BearerToken(r)
	var authority keyauthority.Authority
	var e *fault.Error
	if len(r.Header.Values("Authorization")) != 1 || token == "" {
		e = fault.AuthenticationError("invalid_api_key", "Provide one API key.")
	} else {
		authority, e = s.Gate.AuthenticateRequest(r, token, "inference")
	}
	if e == nil {
		s.bindAuthority(x, authority)
		x.attribution, e = ingress.ParseAttribution(r, authority)
	}
	if e != nil {
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	route, ok := x.request.release.Snapshot().FindRoute(routeSlug)
	if !ok || !x.authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) || !slices.Contains(route.Operations, "realtime") {
		e := fault.ModelNotFound(routeSlug)
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	x.Route = &route
	if e := x.GuardrailSurfaceGate(&route); e != nil {
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	if x.preferences, x.priority, e = routingControls(r, x.authority.Policy); e != nil {
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	if e := UnboundedWorkLimits(x.AdmissionAuthority(x.authority), nil, nil, &route); e != nil {
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), nativeDuplexSession)
	defer cancel()
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		e := fault.ServerError(http.StatusBadRequest, "websocket_upgrade_failed", "The duplex WebSocket upgrade failed.")
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	limit := s.Config.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	client.SetReadLimit(limit)
	setupCtx, stopSetup := context.WithTimeout(ctx, requestBodyTimeout)
	messageType, initial, err := client.Read(setupCtx)
	stopSetup()
	if err != nil || messageType != websocket.MessageText {
		client.Close(websocket.StatusPolicyViolation, "setup required")
		x.Failure, status = fault.InvalidRequest("invalid_duplex_setup", "A native duplex session requires a setup frame first.", nil), http.StatusBadRequest
		return
	}
	setup, inspectErr := duplexcontract.InspectProfileSetup(descriptor, initial, int(limit))
	if inspectErr == nil && setup.Model != "" && setup.Model != routeSlug {
		inspectErr = errDuplexClientProtocol
	}
	if inspectErr == nil {
		inspectErr = duplexcontract.AdmitProfileSetup(descriptor, setup)
	}
	if inspectErr != nil {
		client.Close(websocket.StatusPolicyViolation, "setup refused")
		x.Failure, status = fault.InvalidRequest("invalid_duplex_setup", "The native duplex setup is not admitted by the compiled profile.", nil), http.StatusBadRequest
		return
	}
	if route.MaxBodyBytes != nil && int64(len(initial)) > *route.MaxBodyBytes {
		client.Close(websocket.StatusMessageTooBig, "request body limit exceeded")
		x.Failure, status = ingress.BodyTooLarge(), http.StatusRequestEntityTooLarge
		return
	}
	client.SetReadLimit(bodylimit.Lower(limit, route.MaxBodyBytes))
	if held, exposureErr := s.sessionExposureHeld(ctx, x.authority.ID); exposureErr != nil {
		client.Close(websocket.StatusPolicyViolation, "exposure check unavailable")
		x.Failure, status = exposureErr, exposureErr.Status
		return
	} else if held {
		heldErr := fault.ServerError(http.StatusConflict, "session_exposure_unresolved", "An earlier session for this key is still billing-uncertain; new sessions are refused until that exposure resolves.")
		client.Close(websocket.StatusPolicyViolation, "unresolved session exposure")
		x.Failure, status = heldErr, heldErr.Status
		return
	}
	x.estimate = resourceEstimate
	if e := s.reserveState(ctx, x, x.authority, nativeDuplexSession); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.Failure, status = e, e.Status
		return
	}
	var p *Pin
	defer func() { s.settleSession(r.Context(), x, p) }()
	p, e = s.SelectPinSurface(ctx, x, &route, "realtime", "native", "realtime", func(provider *routing.Provider, model string) bool {
		return provider.ProfileID == descriptor.ProfileID && provider.Connector().Supports("realtime", "native", "realtime") && provider.Supports(model, "realtime", "native", "realtime")
	})
	if e != nil {
		client.Close(websocket.StatusPolicyViolation, "provider unavailable")
		x.Failure, status = e, e.Status
		return
	}
	template, compiled := x.request.release.Snapshot().DuplexTemplate(route.Slug, p.Target.ID)
	if !compiled || template.Dialect() != descriptor.Dialect {
		client.Close(websocket.StatusPolicyViolation, "profile unavailable")
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "upstream_error", "The selected target has no compiled native duplex contract."), http.StatusBadGateway
		return
	}
	if e := s.UnboundedPinLimits(x.AdmissionAuthority(x.authority), p, &route); e != nil {
		client.Close(websocket.StatusPolicyViolation, "unbounded work cannot enforce budgets")
		x.Failure, status = e, e.Status
		return
	}
	if e := s.reserveSessionDebit(ctx, x, x.authority, p.attempt, nil); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.Failure, status = e, e.Status
		return
	}
	if e := s.reserveSessionCost(ctx, x, x.authority, p.attempt, nativeDuplexSession); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.Failure, status = e, e.Status
		return
	}
	if e := s.markSessionDispatched(ctx, x); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.Failure, status = e, e.Status
		return
	}
	upstream, e := s.duplexDial(ctx, x, p, template)
	if e != nil {
		client.Close(websocket.StatusInternalError, "provider unavailable")
		x.Failure, status = e, e.Status
		return
	}
	defer upstream.Close(websocket.StatusNormalClosure, "")
	boundModel, modelErr := p.Provider.Connector().DuplexBoundModel(p.Model)
	bound, bindErr := template.BindSetup(initial, boundModel)
	if modelErr != nil {
		bindErr = modelErr
	}
	if bindErr != nil || int64(len(bound)) > limit {
		client.Close(websocket.StatusPolicyViolation, "invalid setup")
		x.Failure, status = fault.InvalidRequest("invalid_duplex_setup", "The native duplex setup could not be bound.", nil), http.StatusBadRequest
		return
	}
	observer := template.Observer(int(limit))
	if err := template.ObserveSetupFrame(observer, bound); err != nil {
		client.Close(websocket.StatusPolicyViolation, "invalid setup")
		x.Failure, status = fault.InvalidRequest("invalid_duplex_setup", "The native duplex setup could not be bound.", nil), http.StatusBadRequest
		return
	}
	x.dispatched = true
	if err := writeGeminiLiveFrame(ctx, upstream, websocket.MessageText, bound); err != nil {
		client.Close(websocket.StatusInternalError, "setup uncertain")
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "upstream_outcome_unknown", "The duplex setup outcome is unknown."), http.StatusBadGateway
		markDuplexFactUncertain(x, classAmbiguous)
		return
	}
	if template.SetupAck() {
		if ackErr := awaitDuplexSetupAck(ctx, upstream, client, observer, p.attempt.Timeout); ackErr != nil {
			client.Close(websocket.StatusInternalError, "setup uncertain")
			if errors.Is(ackErr, errDuplexProtocol) {
				x.Failure, status = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The duplex provider sent an invalid setup acknowledgement."), http.StatusBadGateway
				markDuplexFactUncertain(x, ClassProtocol)
			} else {
				x.Failure, status = fault.ServerError(http.StatusBadGateway, "upstream_outcome_unknown", "The duplex provider did not acknowledge setup."), http.StatusBadGateway
				markDuplexFactUncertain(x, classAmbiguous)
			}
			return
		}
	}
	x.Delivered(s.Now())
	if len(x.facts) > 0 && x.facts[len(x.facts)-1].Interaction != nil {
		x.facts[len(x.facts)-1].Interaction.UpstreamState = acceptance.Accepted
		x.facts[len(x.facts)-1].Interaction.ClientState = usage.ClientPartial
	}
	usageRecord, usageComplete, relayErr := s.relayNativeDuplex(ctx, x, p, client, upstream, token, observer, template.Transport(), int(limit))
	class := ClassProtocol
	cancelled := false
	if x.Strict() && relayErr != nil {
		class = classAmbiguous
		switch {
		case errors.Is(relayErr, errDuplexProviderError):
			class = ClassUpstreamServer
		case errors.Is(relayErr, errDuplexClientProtocol), errors.Is(relayErr, errDuplexProtocol):
			class = ClassProtocol
		case errors.Is(relayErr, errDuplexAuthority):
			class = classCredential
		case errors.Is(relayErr, errDuplexClientClosed), errors.Is(relayErr, context.Canceled):
			class, cancelled = ClassCancelled, true
		case errors.Is(relayErr, context.DeadlineExceeded):
			class = ClassTimeout
		}
	}
	if len(x.facts) > 0 {
		fact := &x.facts[len(x.facts)-1]
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
	if relayErr != nil {
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "duplex_incomplete", "The duplex session ended without a complete transport contract."), http.StatusBadGateway
		if x.Strict() {
			switch {
			case errors.Is(relayErr, errDuplexProviderError):
				x.Failure = fault.ServerError(http.StatusBadGateway, "duplex_provider_error", "The duplex provider reported an error.")
			case errors.Is(relayErr, errDuplexProtocol):
				x.Failure = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The duplex provider sent an invalid frame.")
			case errors.Is(relayErr, errDuplexClientProtocol):
				x.Failure = fault.InvalidRequest("invalid_duplex_frame", "The duplex client sent an invalid frame.", nil)
			case errors.Is(relayErr, errDuplexAuthority):
				x.Failure = fault.PermissionError("key_revoked", "The duplex session no longer has route authority.")
			case class == ClassCancelled:
				x.Failure = (&AttemptFailure{class: ClassCancelled}).Fault()
			case class == ClassTimeout:
				x.Failure = (&AttemptFailure{class: ClassTimeout}).Fault()
			}
			status = http.StatusSwitchingProtocols
			out = &Outcome{Err: x.Failure, Committed: true, Cancelled: cancelled}
		}
		return
	}
	status, out = http.StatusOK, &Outcome{Committed: true}
	if x.Strict() {
		status = http.StatusSwitchingProtocols
	}
}

func (s *Server) duplexDial(ctx context.Context, x *Call, p *Pin, template *duplexcontract.Template) (duplexPeer, *fault.Error) {
	switch template.Transport() {
	case duplexcontract.TransportBedrockBidi:
		return s.bedrockDuplexDial(ctx, x, p)
	case duplexcontract.TransportWebSocket:
		endpoint, err := p.Provider.Connector().DuplexURL(p.Model)
		if err != nil {
			return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The duplex profile cannot address this model.")
		}
		check := strings.Replace(strings.Replace(endpoint, "wss://", "https://", 1), "ws://", "http://", 1)
		if parsed, parseErr := url.Parse(check); parseErr == nil {
			parsed.RawQuery = ""
			check = parsed.String()
		}
		if _, err := s.egress.ValidateEndpoint(check); err != nil {
			return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The duplex provider address is not permitted.")
		}
		dialCtx, cancel := context.WithCancel(ctx)
		dialTimer := time.AfterFunc(p.attempt.Timeout, cancel)
		conn, e := realtimeDial(dialCtx, s, x, p, endpoint)
		dialTimer.Stop()
		cancel()
		if e != nil {
			return nil, e
		}
		limit := s.Config.MaxEventBytes
		if limit <= 0 {
			limit = 1 << 20
		}
		conn.SetReadLimit(limit)
		return conn, nil
	}
	return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The duplex transport is unavailable.")
}

func (s *Server) relayNativeDuplex(ctx context.Context, x *Call, p *Pin, client, upstream duplexPeer, token string, observer *duplexcontract.Observer, transport duplexcontract.Transport, maxBytes int) (*wire.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type relayEnd struct {
		err            error
		client         bool
		deliveryFailed bool
	}
	forward := func(dst, src duplexPeer, fromProvider bool) relayEnd {
		end := func(err error) relayEnd { return relayEnd{err: err} }
		for {
			kind, payload, err := src.Read(ctx)
			if err != nil {
				return relayEnd{err: err, client: !fromProvider}
			}
			if kind != websocket.MessageText {
				if fromProvider {
					return end(errDuplexProtocol)
				}
				return end(errDuplexClientProtocol)
			}
			providerError := false
			if fromProvider {
				frame, observeErr := observer.ServerFrame(payload)
				if observeErr != nil {
					return end(fmt.Errorf("%w: %v", errDuplexProtocol, observeErr))
				}
				providerError = frame.ProviderError
				if providerError {
					doc, parseErr := oif.ParseJSON(payload, oif.Limits{MaxBytes: maxBytes})
					if parseErr != nil {
						return end(fmt.Errorf("%w: provider error could not be parsed", errDuplexProtocol))
					}
					scrubbed, redactErr := fault.RedactNativeFailureDocument(doc, x.sensitive)
					if redactErr != nil {
						return end(fmt.Errorf("%w: provider error could not be redacted", errDuplexProtocol))
					}
					payload = scrubbed.Bytes()
				}
			} else if err := observer.ClientFrame(payload); err != nil {
				return end(fmt.Errorf("%w: %v", errDuplexClientProtocol, err))
			}
			if err := writeGeminiLiveFrame(ctx, dst, kind, payload); err != nil {
				return relayEnd{err: err, client: fromProvider, deliveryFailed: fromProvider}
			}
			if fromProvider {
				x.Delivered(s.Now())
				if providerError {
					return end(errDuplexProviderError)
				}
			}
		}
	}
	done := make(chan relayEnd, 2)
	relay := func(dst, src duplexPeer, fromProvider bool) {
		defer panics.Recover(s.log, "duplex relay", func(err error) { done <- relayEnd{err: err} })
		done <- forward(dst, src, fromProvider)
	}
	go relay(upstream, client, false)
	go relay(client, upstream, true)
	reauth := time.NewTicker(ingress.ReauthInterval)
	defer reauth.Stop()
	heartbeat := time.NewTicker(realtimePing)
	defer heartbeat.Stop()
	var first relayEnd
	remaining := 2
loop:
	for {
		select {
		case first = <-done:
			remaining--
			break loop
		case <-reauth.C:
			authority, err := s.reauthenticateSession(ctx, x, token)
			if err != nil || authority.ID != x.keyID || !authority.Allows("inference", x.Route.Slug, x.Route.ProjectID, s.Now()) || !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) || s.pinEligibility(p) != routing.Eligible {
				first.err = errDuplexAuthority
				upstream.CloseNow()
				client.Close(websocket.StatusPolicyViolation, "authority revoked")
				break loop
			}
			current := *authority
			current.EndUserDigest = x.endUserDigest
			if e := s.UnboundedPinLimits(x.AdmissionAuthority(current), p, x.Route); e != nil {
				first.err = errDuplexAuthority
				upstream.CloseNow()
				client.Close(websocket.StatusPolicyViolation, "consumption budget changed")
				break loop
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, ingress.ReauthInterval)
			first.err = upstream.Ping(pingCtx)
			if first.err == nil {
				first.err = client.Ping(pingCtx)
				first.client = first.err != nil
			}
			pingCancel()
			if first.err != nil {
				break loop
			}
		case <-ctx.Done():
			first.err = ctx.Err()
			break loop
		}
	}
	cancel()
	code := websocket.CloseStatus(first.err)
	if transport == duplexcontract.TransportBedrockBidi && !first.client && observer.PendingResponse() && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		first.err = fmt.Errorf("%w: %v", errDuplexIncomplete, first.err)
		code = websocket.StatusInternalError
	}
	if errors.Is(first.err, errDuplexProviderError) {
		code = websocket.StatusInternalError
	}
	if code < 0 {
		code = websocket.StatusInternalError
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		upstream.Close(code, "")
		client.Close(code, "")
	} else {
		upstream.CloseNow()
		client.CloseNow()
	}
	deliveryFailed := first.deliveryFailed
	for ; remaining > 0; remaining-- {
		end := <-done
		deliveryFailed = deliveryFailed || end.deliveryFailed
	}
	if transport == duplexcontract.TransportBedrockBidi && first.client && !errors.Is(first.err, context.Canceled) && !errors.Is(first.err, context.DeadlineExceeded) && (observer.PendingResponse() || deliveryFailed) {
		return observer.Usage(), false, fmt.Errorf("%w: %v", errDuplexClientClosed, first.err)
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		return observer.Usage(), observer.Complete(), nil
	}
	return observer.Usage(), false, first.err
}

func awaitDuplexSetupAck(ctx context.Context, upstream, client duplexPeer, observer *duplexcontract.Observer, timeout time.Duration) error {
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
		return fmt.Errorf("%w: setup acknowledgement required", errDuplexProtocol)
	}
	return writeGeminiLiveFrame(ctx, client, kind, ack)
}

func markDuplexFactUncertain(x *Call, class string) {
	if len(x.facts) == 0 {
		return
	}
	fact := &x.facts[len(x.facts)-1]
	fact.Class, fact.BillingUncertain = class, true
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = acceptance.Unknown
	}
}
