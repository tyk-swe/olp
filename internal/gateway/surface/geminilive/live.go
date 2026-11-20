package geminilive

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/routing"
	acceptance "github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

const liveSession = time.Hour

const livePing = 30 * time.Second

var (
	errLiveProviderError  = errors.New("Gemini Live provider reported an error")
	errLiveIncomplete     = errors.New("Gemini Live provider closed with work in flight")
	errLiveClientClosed   = errors.New("Gemini Live client closed with work in flight")
	errLiveProtocol       = errors.New("Gemini Live provider protocol violation")
	errLiveClientProtocol = errors.New("Gemini Live client protocol violation")
	errLiveAuthority      = errors.New("Gemini Live authority revoked")
)

type closeError struct {
	fault   *fault.Error
	code    websocket.StatusCode
	reason  string
	noClose bool
}

func policyClose(e *fault.Error, reason string) *closeError {
	return &closeError{fault: e, code: websocket.StatusPolicyViolation, reason: reason}
}

type liveTurn struct {
	surface
	w         http.ResponseWriter
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	token     string
	end       *liveEnd
}

type liveEnd struct {
	pin    *kernel.Pin
	status int
	out    *kernel.Outcome
}

func (t liveTurn) fail(e *fault.Error) {
	t.x.Failure, t.end.status = e, e.Status
	fault.WriteSurfaceError(t.w, e, "gemini")
}

func (t liveTurn) failClose(client *websocket.Conn, ce *closeError) {
	if !ce.noClose {
		client.Close(ce.code, ce.reason)
	}
	t.x.Failure, t.end.status = ce.fault, ce.fault.Status
}

func (s surface) live(w http.ResponseWriter, r *http.Request) {
	x := s.kernel.SessionCall(w, r, wire.GeminiLive, "realtime")
	end := &liveEnd{status: http.StatusInternalServerError}
	defer func() { s.kernel.Finish(x, end.out, end.status) }()
	fail := func(e *fault.Error) {
		x.Failure, end.status = e, e.Status
		fault.WriteSurfaceError(w, e, "gemini")
	}
	if !s.kernel.Gate.Admit(r.Context()) {
		fail(fault.Overloaded)
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	authority, token, e := s.admitLiveCaller(x, r)
	if e != nil {
		fail(e)
		return
	}
	t := liveTurn{surface: s, w: w, r: r, x: x, authority: authority, token: token, end: end}
	ctx, cancel := context.WithTimeout(r.Context(), liveSession)
	defer cancel()
	client, e := t.acceptClient()
	if e != nil {
		t.fail(e)
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	setup, _, limit, route, ce := t.readSetup(ctx, client)
	if ce != nil {
		t.failClose(client, ce)
		return
	}
	x.SessionEstimate()
	if e := s.kernel.ReserveState(ctx, x, authority, liveSession); e != nil {
		t.failClose(client, policyClose(e, "admission refused"))
		return
	}
	defer func() { s.kernel.SettleSession(r.Context(), x, t.end.pin) }()
	pin, ce := t.pinAndReserve(ctx, route)
	if ce != nil {
		t.failClose(client, ce)
		return
	}
	upstream, ce := t.dispatchSetup(ctx, cancel, client, pin, setup, limit)
	if upstream != nil {
		defer upstream.Close(websocket.StatusNormalClosure, "")
	}
	if ce != nil {
		t.failClose(client, ce)
		return
	}
	usageRecord, complete, relayErr := s.relayLive(ctx, x, pin, client, upstream, token, int(limit))
	t.finishRelay(usageRecord, complete, relayErr)
}

func (s surface) admitLiveCaller(x *kernel.Call, r *http.Request) (keyauthority.Authority, string, *fault.Error) {
	if !s.kernel.BrowserOriginAllowed(r) {
		return keyauthority.Authority{}, "", fault.PermissionError("origin_forbidden", "This origin is not allowed to open Live sessions.")
	}
	if !kernel.RealtimeUpgrade(r) {
		return keyauthority.Authority{}, "", fault.InvalidRequest("websocket_upgrade_required", "Gemini Live requires a WebSocket upgrade.", nil)
	}
	token, e := kernel.GeminiClientKey(r)
	if e != nil {
		return keyauthority.Authority{}, "", e
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

func (t liveTurn) acceptClient() (*websocket.Conn, *fault.Error) {
	// Like a realtime upgrade, this one carries no allowance or metadata headers.
	client, err := websocket.Accept(t.w, t.r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, fault.ServerError(http.StatusBadRequest, "websocket_upgrade_failed", "The Live WebSocket upgrade failed.")
	}
	return client, nil
}

func (t liveTurn) readSetup(ctx context.Context, client *websocket.Conn) (geminilifecycle.LiveSetup, []byte, int64, *routing.Route, *closeError) {
	limit := t.kernel.Config.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	client.SetReadLimit(limit)
	setupCtx, stopSetup := context.WithTimeout(ctx, kernel.RequestBodyTimeout)
	messageType, initial, err := client.Read(setupCtx)
	stopSetup()
	if err != nil || messageType != websocket.MessageText {
		return geminilifecycle.LiveSetup{}, nil, limit, nil, &closeError{fault: fault.InvalidRequest("invalid_live_setup", "Gemini Live requires a setup message first.", nil), code: websocket.StatusPolicyViolation, reason: "setup required"}
	}
	setup, err := geminilifecycle.ParseLiveSetup(initial, int(limit))
	if err != nil || !routeidentity.ValidSlug(setup.Model) {
		return geminilifecycle.LiveSetup{}, nil, limit, nil, &closeError{fault: fault.InvalidRequest("invalid_live_setup", "Gemini Live setup is malformed.", nil), code: websocket.StatusPolicyViolation, reason: "invalid setup"}
	}
	route, ok := t.x.Snapshot().FindRoute(setup.Model)
	if !ok || !t.authority.Allows("inference", route.Slug, route.ProjectID, t.kernel.Now()) || !slices.Contains(route.Operations, "realtime") {
		return geminilifecycle.LiveSetup{}, nil, limit, nil, &closeError{fault: fault.ModelNotFound(setup.Model), code: websocket.StatusPolicyViolation, reason: "route unavailable"}
	}
	t.x.Route = &route
	if route.MaxBodyBytes != nil && int64(len(initial)) > *route.MaxBodyBytes {
		return geminilifecycle.LiveSetup{}, nil, limit, nil, &closeError{fault: ingress.BodyTooLarge(), code: websocket.StatusMessageTooBig, reason: "request body limit exceeded"}
	}
	client.SetReadLimit(bodylimit.Lower(limit, route.MaxBodyBytes))
	if e := t.x.GuardrailSurfaceGate(&route); e != nil {
		return geminilifecycle.LiveSetup{}, nil, limit, nil, policyClose(e, "policy unavailable")
	}
	if e := t.x.RoutingControls(t.r, t.authority); e != nil {
		return geminilifecycle.LiveSetup{}, nil, limit, nil, policyClose(e, "invalid routing")
	}
	if e := kernel.UnboundedWorkLimits(t.x.AdmissionAuthority(t.x.Authority()), nil, nil, &route); e != nil {
		return geminilifecycle.LiveSetup{}, nil, limit, nil, policyClose(e, "unbounded work cannot enforce budgets")
	}
	return setup, initial, limit, &route, nil
}

func (t liveTurn) pinAndReserve(ctx context.Context, route *routing.Route) (*kernel.Pin, *closeError) {
	p, e := t.kernel.SelectPinSurface(ctx, t.x, route, "realtime", "gemini", "realtime", func(provider *routing.Provider, model string) bool {
		return provider.ProfileID == "gemini-live" && provider.Connector().Supports("realtime", "gemini", "realtime") && provider.Supports(model, "realtime", "gemini", "realtime")
	})
	t.end.pin = p
	if e != nil {
		return p, policyClose(e, "provider unavailable")
	}
	if e := t.kernel.UnboundedPinLimits(t.x.AdmissionAuthority(t.x.Authority()), p, route); e != nil {
		return p, policyClose(e, "unbounded work cannot enforce budgets")
	}
	if e := t.kernel.ReserveSessionCost(ctx, t.x, t.x.Authority(), p, liveSession); e != nil {
		return p, policyClose(e, "admission refused")
	}
	return p, nil
}

func (t liveTurn) dispatchSetup(ctx context.Context, cancel context.CancelFunc, client *websocket.Conn, p *kernel.Pin, setup geminilifecycle.LiveSetup, limit int64) (*websocket.Conn, *closeError) {
	endpoint, err := p.Provider.Connector().GeminiLiveURL()
	if err != nil {
		return nil, policyClose(fault.ServerError(http.StatusBadGateway, "upstream_error", "The Live profile cannot address this model."), "profile unavailable")
	}
	check := strings.Replace(strings.Replace(endpoint, "wss://", "https://", 1), "ws://", "http://", 1)
	if _, err := t.kernel.Egress.ValidateEndpoint(check); err != nil {
		return nil, policyClose(fault.ServerError(http.StatusBadGateway, "upstream_error", "The Live provider address is not permitted."), "provider unavailable")
	}
	bound, err := setup.BindModel(p.Provider.Connector().Model(p.Model))
	if err != nil || int64(len(bound)) > limit {
		return nil, &closeError{fault: fault.InvalidRequest("invalid_live_setup", "Gemini Live setup could not be bound.", nil), code: websocket.StatusPolicyViolation, reason: "invalid setup"}
	}
	dialTimer := time.AfterFunc(p.Timeout(), cancel)
	upstream, e := t.kernel.RealtimeDial(ctx, t.x, p, endpoint)
	dialTimer.Stop()
	if e != nil {
		return nil, &closeError{fault: e, code: websocket.StatusInternalError, reason: "provider unavailable"}
	}
	upstream.SetReadLimit(limit)
	// A failed setup write may still have reached the provider. This request
	// owns one Attempt and never selects another provider after that boundary.
	t.x.MarkDispatched()
	if err := kernel.WriteDuplexFrame(ctx, upstream, websocket.MessageText, bound); err != nil {
		markFactUncertain(t.x, kernel.ClassAmbiguous)
		return upstream, &closeError{fault: fault.ServerError(http.StatusBadGateway, "upstream_outcome_unknown", "The Live setup outcome is unknown."), code: websocket.StatusInternalError, reason: "setup uncertain"}
	}
	if ce := t.awaitAck(ctx, client, upstream, p, limit); ce != nil {
		return upstream, ce
	}
	return upstream, nil
}

func (t liveTurn) awaitAck(ctx context.Context, client, upstream *websocket.Conn, p *kernel.Pin, limit int64) *closeError {
	ackCtx, stopAck := context.WithTimeout(ctx, p.Timeout())
	ackType, ack, err := upstream.Read(ackCtx)
	stopAck()
	if err != nil {
		markFactUncertain(t.x, kernel.ClassAmbiguous)
		return &closeError{fault: fault.ServerError(http.StatusBadGateway, "upstream_outcome_unknown", "The Live provider did not acknowledge setup."), code: websocket.StatusInternalError, reason: "setup uncertain"}
	}
	if ackType != websocket.MessageText || geminilifecycle.ValidateLiveServerFrame(ack, int(limit)) != nil {
		if fact := t.x.LastAttemptFact(); fact != nil {
			fact.Class, fact.BillingUncertain = kernel.ClassProtocol, true
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = acceptance.Terminal
			}
		}
		return &closeError{fault: fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent a malformed setup acknowledgement."), code: websocket.StatusProtocolError, reason: "invalid setup acknowledgement"}
	}
	ackDocument, err := oif.ParseJSON(ack, oif.Limits{MaxBytes: int(limit)})
	if err != nil {
		return &closeError{fault: fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent an invalid setup acknowledgement."), code: websocket.StatusProtocolError, reason: "invalid setup acknowledgement"}
	}
	if _, ok := ackDocument.Root().Lookup("setupComplete"); !ok {
		return &closeError{fault: fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent content before setup completion."), code: websocket.StatusProtocolError, reason: "setup acknowledgement required"}
	}
	if err := kernel.WriteDuplexFrame(ctx, client, websocket.MessageText, ack); err != nil {
		return &closeError{fault: fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The Live setup acknowledgement could not be delivered."), noClose: true}
	}
	t.x.Delivered(t.kernel.Now())
	if fact := t.x.LastAttemptFact(); fact != nil && fact.Interaction != nil {
		fact.Interaction.UpstreamState = acceptance.Accepted
		fact.Interaction.ClientState = usage.ClientPartial
	}
	return nil
}

func (t liveTurn) finishRelay(usageRecord *wire.Usage, usageComplete bool, relayErr error) {
	x := t.x
	class := kernel.ClassProtocol
	cancelled := false
	if x.Strict() && relayErr != nil {
		class = kernel.ClassAmbiguous
		switch {
		case errors.Is(relayErr, errLiveProviderError):
			class = kernel.ClassUpstreamServer
		case errors.Is(relayErr, errLiveClientClosed):
			class, cancelled = kernel.ClassCancelled, true
		case errors.Is(relayErr, errLiveClientProtocol), errors.Is(relayErr, errLiveProtocol):
			class = kernel.ClassProtocol
		case errors.Is(relayErr, errLiveAuthority):
			class = kernel.ClassCredential
		case errors.Is(relayErr, context.Canceled):
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
				if errors.Is(relayErr, errLiveProviderError) {
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
	x.Failure, t.end.status = fault.ServerError(http.StatusBadGateway, "live_incomplete", "The Live session ended without a complete transport contract."), http.StatusBadGateway
	if !x.Strict() {
		return
	}
	switch {
	case errors.Is(relayErr, errLiveProviderError):
		x.Failure = fault.ServerError(http.StatusBadGateway, "live_provider_error", "The Live provider reported an error.")
	case errors.Is(relayErr, errLiveProtocol):
		x.Failure = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent an invalid frame.")
	case errors.Is(relayErr, errLiveClientProtocol):
		x.Failure = fault.InvalidRequest("invalid_live_frame", "The Live client sent an invalid frame.", nil)
	case errors.Is(relayErr, errLiveAuthority):
		x.Failure = fault.PermissionError("key_revoked", "The Live session no longer has route authority.")
	case class == kernel.ClassCancelled:
		x.Failure = kernel.ClassFailure(kernel.ClassCancelled)
	case class == kernel.ClassTimeout:
		x.Failure = kernel.ClassFailure(kernel.ClassTimeout)
	}
	t.end.status = http.StatusSwitchingProtocols
	t.end.out = &kernel.Outcome{Err: x.Failure, Committed: true, Cancelled: cancelled}
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
