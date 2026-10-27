package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

const nativeDuplexSession = time.Hour

var (
	errDuplexProviderError  = errors.New("native duplex provider reported an error")
	errDuplexProtocol       = errors.New("native duplex provider protocol violation")
	errDuplexClientProtocol = errors.New("native duplex client protocol violation")
	errDuplexAuthority      = errors.New("native duplex authority revoked")
)

// Native duplex sessions are connection scoped. The gateway mints no session
// identifier and exposes no follow-up call that could address one, so owner
// scoping is the authenticated connection itself, opened under one key
// against one published route. Provider session identifiers inside relayed
// frames stay opaque payload and mean nothing at the gateway boundary.
func (s *Server) nativeDuplex(w http.ResponseWriter, r *http.Request) {
	dialect, routeSlug := r.PathValue("dialect"), r.PathValue("route")
	x := &call{request: s.begin(w, r), family: wire.NativeDuplex, actor: "api_key", mode: "realtime"}
	status := http.StatusInternalServerError
	var out *outcome
	defer func() { s.finish(x, out, status) }()
	surface := requestSurface(r)
	descriptor, known := duplexcontract.Lookup(dialect)
	if !known {
		e := notFoundError("not_found", "Unknown native duplex dialect.")
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, surface)
		return
	}
	if !s.admit(r.Context()) {
		x.failure, status = overloaded, overloaded.Status
		writeSurfaceError(w, overloaded, surface)
		return
	}
	defer s.release(r.Context())
	if !s.browserOriginAllowed(r) {
		e := permissionError("origin_forbidden", "This origin is not allowed to open duplex sessions.")
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, surface)
		return
	}
	if !realtimeUpgrade(r) {
		e := invalidRequest("websocket_upgrade_required", "Native duplex sessions require a WebSocket upgrade.", nil)
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, surface)
		return
	}
	token := bearerToken(r)
	var authority access.Authority
	var e *Error
	if len(r.Header.Values("Authorization")) != 1 || token == "" {
		e = authenticationError("invalid_api_key", "Provide one API key.")
	} else {
		authority, e = s.authenticateRequest(r, token, "inference")
	}
	if e == nil {
		s.bindAuthority(x, authority)
		x.attribution, e = s.parseAttribution(r, authority)
	}
	if e != nil {
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, surface)
		return
	}
	route, ok := x.request.release.Snapshot.Routes[routeSlug]
	if !ok || !x.authority.Allows("inference", route.Slug, route.ProjectID, s.now()) || !slices.Contains(route.Operations, "realtime") {
		e := modelNotFound(routeSlug)
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, surface)
		return
	}
	x.route = &route
	if e := x.guardrailSurfaceGate(&route); e != nil {
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, surface)
		return
	}
	if x.preferences, x.priority, e = routingControls(r, x.authority.Policy); e != nil {
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, surface)
		return
	}
	if e := unboundedWorkLimits(x.admissionAuthority(x.authority), nil, nil, &route); e != nil {
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, surface)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), nativeDuplexSession)
	defer cancel()
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		e := serverError(http.StatusBadRequest, "websocket_upgrade_failed", "The duplex WebSocket upgrade failed.")
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, surface)
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	limit := s.cfg.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	client.SetReadLimit(limit)
	setupCtx, stopSetup := context.WithTimeout(ctx, requestBodyTimeout)
	messageType, initial, err := client.Read(setupCtx)
	stopSetup()
	if err != nil || messageType != websocket.MessageText {
		client.Close(websocket.StatusPolicyViolation, "setup required")
		x.failure, status = invalidRequest("invalid_duplex_setup", "A native duplex session requires a setup frame first.", nil), http.StatusBadRequest
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
		x.failure, status = invalidRequest("invalid_duplex_setup", "The native duplex setup is not admitted by the compiled profile.", nil), http.StatusBadRequest
		return
	}
	if route.MaxBodyBytes != nil && int64(len(initial)) > *route.MaxBodyBytes {
		client.Close(websocket.StatusMessageTooBig, "request body limit exceeded")
		x.failure, status = bodyTooLarge(), http.StatusRequestEntityTooLarge
		return
	}
	client.SetReadLimit(bodylimit.Lower(limit, route.MaxBodyBytes))
	if held, exposureErr := s.sessionExposureHeld(ctx, x.authority.ID); exposureErr != nil {
		client.Close(websocket.StatusPolicyViolation, "exposure check unavailable")
		x.failure, status = exposureErr, exposureErr.Status
		return
	} else if held {
		heldErr := serverError(http.StatusConflict, "session_exposure_unresolved", "An earlier session for this key is still billing-uncertain; new sessions are refused until that exposure resolves.")
		client.Close(websocket.StatusPolicyViolation, "unresolved session exposure")
		x.failure, status = heldErr, heldErr.Status
		return
	}
	x.estimate = resourceEstimate
	if e := s.reserveState(ctx, x, x.authority, nativeDuplexSession); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.failure, status = e, e.Status
		return
	}
	var p *pin
	defer func() {
		s.finishSessionDebit(r.Context(), x, x.failure != nil)
		settled := geminiLiveSettlement(x)
		if p != nil {
			s.settlePinHold(r.Context(), x, p.hold, settled)
		}
		s.settleCaps(r.Context(), x)
		s.recordCost(x)
		settleKey(r.Context(), x.lease, x.dispatched, settled, s.log)
	}()
	p, e = s.selectPinSurface(ctx, x, &route, "realtime", "native", "realtime", func(provider *routing.Provider, model string) bool {
		return provider.ProfileID == descriptor.ProfileID && provider.Connector().Supports("realtime", "native", "realtime") && provider.Supports(model, "realtime", "native", "realtime")
	})
	if e != nil {
		client.Close(websocket.StatusPolicyViolation, "provider unavailable")
		x.failure, status = e, e.Status
		return
	}
	template, compiled := x.request.release.Snapshot.DuplexTemplate(route.Slug, p.target.ID)
	if !compiled || template.Dialect() != descriptor.Dialect {
		client.Close(websocket.StatusPolicyViolation, "profile unavailable")
		x.failure, status = serverError(http.StatusBadGateway, "upstream_error", "The selected target has no compiled native duplex contract."), http.StatusBadGateway
		return
	}
	if e := s.unboundedPinLimits(x.admissionAuthority(x.authority), p, &route); e != nil {
		client.Close(websocket.StatusPolicyViolation, "unbounded work cannot enforce budgets")
		x.failure, status = e, e.Status
		return
	}
	if e := s.reserveSessionDebit(ctx, x, x.authority, p.attempt); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.failure, status = e, e.Status
		return
	}
	if e := s.reserveSessionCost(ctx, x, x.authority, p.attempt, nativeDuplexSession); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.failure, status = e, e.Status
		return
	}
	if e := s.markSessionDispatched(ctx, x); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.failure, status = e, e.Status
		return
	}
	upstream, e := s.duplexDial(ctx, x, p, template)
	if e != nil {
		client.Close(websocket.StatusInternalError, "provider unavailable")
		x.failure, status = e, e.Status
		return
	}
	defer upstream.Close(websocket.StatusNormalClosure, "")
	boundModel, modelErr := p.provider.Connector().DuplexBoundModel(p.model)
	bound, bindErr := template.BindSetup(initial, boundModel)
	if modelErr != nil {
		bindErr = modelErr
	}
	if bindErr != nil || int64(len(bound)) > limit {
		client.Close(websocket.StatusPolicyViolation, "invalid setup")
		x.failure, status = invalidRequest("invalid_duplex_setup", "The native duplex setup could not be bound.", nil), http.StatusBadRequest
		return
	}
	x.dispatched = true
	if err := writeGeminiLiveFrame(ctx, upstream, websocket.MessageText, bound); err != nil {
		client.Close(websocket.StatusInternalError, "setup uncertain")
		x.failure, status = serverError(http.StatusBadGateway, "upstream_outcome_unknown", "The duplex setup outcome is unknown."), http.StatusBadGateway
		markDuplexFactUncertain(x, classAmbiguous)
		return
	}
	observer := template.Observer(int(limit))
	if template.SetupAck() {
		if ackErr := awaitDuplexSetupAck(ctx, upstream, client, observer, p.attempt.Timeout); ackErr != nil {
			client.Close(websocket.StatusInternalError, "setup uncertain")
			if errors.Is(ackErr, errDuplexProtocol) {
				x.failure, status = serverError(http.StatusBadGateway, "provider_protocol_error", "The duplex provider sent an invalid setup acknowledgement."), http.StatusBadGateway
				markDuplexFactUncertain(x, classProtocol)
			} else {
				x.failure, status = serverError(http.StatusBadGateway, "upstream_outcome_unknown", "The duplex provider did not acknowledge setup."), http.StatusBadGateway
				markDuplexFactUncertain(x, classAmbiguous)
			}
			return
		}
	}
	x.delivered(s.now())
	if len(x.facts) > 0 && x.facts[len(x.facts)-1].Interaction != nil {
		x.facts[len(x.facts)-1].Interaction.UpstreamState = usage.UpstreamAccepted
		x.facts[len(x.facts)-1].Interaction.ClientState = usage.ClientPartial
	}
	usageRecord, usageComplete, relayErr := s.relayNativeDuplex(ctx, x, p, client, upstream, token, observer, int(limit))
	class := classProtocol
	cancelled := false
	if x.strict() && relayErr != nil {
		class = classAmbiguous
		switch {
		case errors.Is(relayErr, errDuplexProviderError):
			class = classUpstreamServer
		case errors.Is(relayErr, errDuplexClientProtocol), errors.Is(relayErr, errDuplexProtocol):
			class = classProtocol
		case errors.Is(relayErr, errDuplexAuthority):
			class = classCredential
		case errors.Is(relayErr, context.Canceled):
			class, cancelled = classCancelled, true
		case errors.Is(relayErr, context.DeadlineExceeded):
			class = classTimeout
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
					fact.Interaction.UpstreamState = usage.UpstreamTerminal
				} else {
					fact.Interaction.UpstreamState = usage.UpstreamUnknown
				}
			}
		} else if fact.Interaction != nil {
			fact.Interaction.UpstreamState, fact.Interaction.ClientState = usage.UpstreamTerminal, usage.ClientTerminal
		}
	}
	if relayErr != nil {
		x.failure, status = serverError(http.StatusBadGateway, "duplex_incomplete", "The duplex session ended without a complete transport contract."), http.StatusBadGateway
		if x.strict() {
			switch {
			case errors.Is(relayErr, errDuplexProviderError):
				x.failure = serverError(http.StatusBadGateway, "duplex_provider_error", "The duplex provider reported an error.")
			case errors.Is(relayErr, errDuplexProtocol):
				x.failure = serverError(http.StatusBadGateway, "provider_protocol_error", "The duplex provider sent an invalid frame.")
			case errors.Is(relayErr, errDuplexClientProtocol):
				x.failure = invalidRequest("invalid_duplex_frame", "The duplex client sent an invalid frame.", nil)
			case errors.Is(relayErr, errDuplexAuthority):
				x.failure = permissionError("key_revoked", "The duplex session no longer has route authority.")
			case class == classCancelled:
				x.failure = (&attemptFailure{class: classCancelled}).toError()
			case class == classTimeout:
				x.failure = (&attemptFailure{class: classTimeout}).toError()
			}
			status = http.StatusSwitchingProtocols
			out = &outcome{err: x.failure, committed: true, cancelled: cancelled}
		}
		return
	}
	status, out = http.StatusOK, &outcome{committed: true}
	if x.strict() {
		status = http.StatusSwitchingProtocols
	}
}

func (s *Server) duplexDial(ctx context.Context, x *call, p *pin, template *duplexcontract.Template) (duplexPeer, *Error) {
	switch template.Transport() {
	case duplexcontract.TransportBedrockBidi:
		return s.bedrockDuplexDial(ctx, x, p)
	case duplexcontract.TransportWebSocket:
		endpoint, err := p.provider.Connector().DuplexURL(p.model)
		if err != nil {
			return nil, serverError(http.StatusBadGateway, "upstream_error", "The duplex profile cannot address this model.")
		}
		check := strings.Replace(strings.Replace(endpoint, "wss://", "https://", 1), "ws://", "http://", 1)
		if parsed, parseErr := url.Parse(check); parseErr == nil {
			parsed.RawQuery = ""
			check = parsed.String()
		}
		if _, err := s.egress.ValidateEndpoint(check); err != nil {
			return nil, serverError(http.StatusBadGateway, "upstream_error", "The duplex provider address is not permitted.")
		}
		dialCtx, cancel := context.WithCancel(ctx)
		dialTimer := time.AfterFunc(p.attempt.Timeout, cancel)
		conn, e := realtimeDial(dialCtx, s, x, p, endpoint)
		dialTimer.Stop()
		cancel()
		if e != nil {
			return nil, e
		}
		limit := s.cfg.MaxEventBytes
		if limit <= 0 {
			limit = 1 << 20
		}
		conn.SetReadLimit(limit)
		return conn, nil
	}
	return nil, serverError(http.StatusBadGateway, "upstream_error", "The duplex transport is unavailable.")
}

func (s *Server) relayNativeDuplex(ctx context.Context, x *call, p *pin, client, upstream duplexPeer, token string, observer *duplexcontract.Observer, maxBytes int) (*wire.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type relayEnd struct {
		err          error
		fromProvider bool
	}
	forward := func(dst, src duplexPeer, fromProvider bool) relayEnd {
		end := func(err error) relayEnd { return relayEnd{err: err, fromProvider: fromProvider} }
		for {
			kind, payload, err := src.Read(ctx)
			if err != nil {
				return end(err)
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
					scrubbed, redactErr := redactNativeFailureDocument(doc, x.sensitive)
					if redactErr != nil {
						return end(fmt.Errorf("%w: provider error could not be redacted", errDuplexProtocol))
					}
					payload = scrubbed.Bytes()
				}
			} else if err := observer.ClientFrame(payload); err != nil {
				return end(fmt.Errorf("%w: %v", errDuplexClientProtocol, err))
			}
			if err := writeGeminiLiveFrame(ctx, dst, kind, payload); err != nil {
				return end(err)
			}
			if fromProvider {
				x.delivered(s.now())
				if providerError {
					return end(errDuplexProviderError)
				}
			}
		}
	}
	done := make(chan relayEnd, 2)
	go func() { done <- forward(upstream, client, false) }()
	go func() { done <- forward(client, upstream, true) }()
	reauth := time.NewTicker(realtimeReauth)
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
			authority, err := s.Runtime.Authenticate(token)
			if err != nil || authority.ID != x.keyID || !authority.Allows("inference", x.route.Slug, x.route.ProjectID, s.now()) || !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) || s.pinEligibility(p) != routing.Eligible {
				first.err = errDuplexAuthority
				client.Close(websocket.StatusPolicyViolation, "authority revoked")
				break loop
			}
			current := *authority
			current.EndUserDigest = x.endUserDigest
			if e := s.unboundedPinLimits(x.admissionAuthority(current), p, x.route); e != nil {
				first.err = errDuplexAuthority
				client.Close(websocket.StatusPolicyViolation, "consumption budget changed")
				break loop
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, realtimeReauth)
			first.err = upstream.Ping(pingCtx)
			if first.err == nil {
				first.err = client.Ping(pingCtx)
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
	for ; remaining > 0; remaining-- {
		<-done
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

func markDuplexFactUncertain(x *call, class string) {
	if len(x.facts) == 0 {
		return
	}
	fact := &x.facts[len(x.facts)-1]
	fact.Class, fact.BillingUncertain = class, true
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamUnknown
	}
}
