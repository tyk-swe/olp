package realtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/domain/attribution"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/duplexrelay"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

const realtimeSession = time.Hour

const realtimePing = 30 * time.Second

var errRealtimeAuthorityRevoked = errors.New("realtime authority revoked")
var errRealtimeClientClosed = errors.New("realtime client disconnected")
var errRealtimeResponseIncomplete = errors.New("realtime response ended before its terminal event")

// realtimeCredentialError ends a realtime session whose provider credential
// is no longer eligible; the session's failure names why.
type realtimeCredentialError struct{ eligibility routing.Eligibility }

func (e *realtimeCredentialError) Error() string {
	return "realtime provider credential " + string(e.eligibility)
}

type sessionTurn struct {
	surface
	w         http.ResponseWriter
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	token     string
	end       *sessionEnd
}

type sessionEnd struct {
	pin *kernel.Pin
}

func (t sessionTurn) refuse(e *fault.Error) { t.kernel.Refuse(t.x, t.w, e) }

func (s surface) realtime(w http.ResponseWriter, r *http.Request) {
	x := s.kernel.SessionCall(w, r, wire.Realtime, "realtime")
	if !s.kernel.Gate.Admit(r.Context()) {
		s.kernel.Refuse(x, w, fault.Overloaded)
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	authority, token, e := s.admitRealtimeCaller(x, r)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	t := sessionTurn{surface: s, w: w, r: r, x: x, authority: authority, token: token, end: &sessionEnd{}}
	route, e := t.route()
	if e != nil {
		t.refuse(e)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), realtimeSession)
	defer cancel()
	x.SessionEstimate()
	defer func() { s.kernel.SettleSession(r.Context(), x, t.end.pin) }()
	pin, e := t.pin(ctx, route)
	if e != nil {
		t.refuse(e)
		return
	}
	if e := t.reserve(ctx, route); e != nil {
		t.refuse(e)
		return
	}
	t.connectAndRelay(ctx, route, pin)
}

func (s surface) admitRealtimeCaller(x *kernel.Call, r *http.Request) (keyauthority.Authority, string, *fault.Error) {
	if !s.kernel.BrowserOriginAllowed(r) {
		return keyauthority.Authority{}, "", fault.PermissionError("origin_forbidden", "This origin is not allowed to open realtime sessions.")
	}
	if !duplexrelay.RealtimeUpgrade(r) {
		return keyauthority.Authority{}, "", fault.InvalidRequest("websocket_upgrade_required", "This endpoint requires a WebSocket upgrade request.", nil)
	}
	token := ingress.BearerToken(r)
	if token == "" {
		return keyauthority.Authority{}, "", fault.AuthenticationError("missing_authorization", "Provide an Authorization bearer API key.")
	}
	authority, e := s.kernel.Gate.AuthenticateRequest(r, token, "inference")
	if e != nil {
		return keyauthority.Authority{}, "", e
	}
	s.kernel.BindAuthority(x, authority)
	values := r.Header.Values(attribution.Header)
	if len(values) == 0 {
		values = r.URL.Query()["attribution"]
	}
	if e := x.NoteAttributionValues(values, authority); e != nil {
		return keyauthority.Authority{}, "", e
	}
	return authority, token, nil
}

func (t sessionTurn) route() (*routing.Route, *fault.Error) {
	slug := t.r.URL.Query().Get("model")
	if !routeidentity.ValidSlug(slug) {
		return nil, fault.InvalidRequest("invalid_value", "model must name a published route slug.", new("model"))
	}
	snapshot := t.x.Snapshot()
	route, ok := snapshot.FindRoute(slug)
	if !ok {
		return nil, fault.ModelNotFound(slug)
	}
	t.x.Route = &route
	if !t.authority.Allows("inference", route.Slug, route.ProjectID, t.kernel.Now()) {
		return nil, fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
	}
	if !slices.Contains(route.Operations, "realtime") {
		return nil, fault.InvalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow realtime sessions.", nil)
	}
	if e := t.x.GuardrailSurfaceGate(&route); e != nil {
		return nil, e
	}
	if t.x.Strict() {
		if e := strictRealtimeHandshake(snapshot, &route, t.r); e != nil {
			return nil, e
		}
	}
	return &route, nil
}

func (t sessionTurn) pin(ctx context.Context, route *routing.Route) (*kernel.Pin, *fault.Error) {
	var p *kernel.Pin
	var e *fault.Error
	if t.x.Strict() {
		// Realtime has an operation-owned duplex contract. Retained Responses
		// eligibility and the unary codec say nothing about a native session.
		p, e = t.kernel.SelectPinSurface(ctx, t.x, route, "realtime", "openai", "realtime", func(provider *routing.Provider, model string) bool {
			return provider.Connector().Supports("realtime", "openai", "realtime") && provider.Supports(model, "realtime", "openai", "realtime")
		})
	} else {
		p, e = t.kernel.SelectPin(ctx, t.x, route, "realtime", "realtime")
	}
	t.end.pin = p
	if e != nil {
		return p, e
	}
	if t.x.Strict() {
		if _, ok := t.x.Snapshot().RealtimeTemplate(route.Slug, p.Target.ID); !ok {
			return p, kernel.PolicyUnavailable("realtime_contract_unavailable", "The selected target has no compiled strict realtime contract.")
		}
	}
	return p, nil
}

func (t sessionTurn) reserve(ctx context.Context, route *routing.Route) *fault.Error {
	if e := t.kernel.ReserveState(ctx, t.x, t.authority, realtimeSession); e != nil {
		return e
	}
	if e := t.kernel.ReserveSessionCost(ctx, t.x, t.authority, t.end.pin, realtimeSession); e != nil {
		return e
	}
	return nil
}

func (t sessionTurn) dialURL(p *kernel.Pin) (string, *fault.Error) {
	dialURL, e := realtimeURL(p)
	if e != nil {
		return "", e
	}
	check := strings.Replace(dialURL, "wss://", "https://", 1)
	check = strings.Replace(check, "ws://", "http://", 1)
	if u, err := url.Parse(check); err == nil {
		u.RawQuery, u.Fragment = "", ""
		check = u.String()
	}
	if _, err := t.kernel.Egress.ValidateEndpoint(check); err != nil {
		return "", fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy.")
	}
	return dialURL, nil
}

func (t sessionTurn) connectAndRelay(ctx context.Context, route *routing.Route, p *kernel.Pin) {
	dialURL, e := t.dialURL(p)
	if e != nil {
		t.refuse(e)
		return
	}
	// The upgrade carries no allowance or metadata headers. The provider is not
	// dialled until the session is accepted, so there is no attempt to describe,
	// and a WebSocket client does not read the rate-limit headers of an HTTP API.
	client, err := websocket.Accept(t.w, t.r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		t.refuse(fault.ServerError(http.StatusBadRequest, "websocket_upgrade_failed", "The WebSocket upgrade could not be completed."))
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	terminalStatus := http.StatusOK
	if t.x.Strict() {
		// A strict WebSocket has already committed HTTP 101 at this boundary.
		// Later transport errors are carried by close frames and the Attempt.
		terminalStatus = http.StatusSwitchingProtocols
	}
	conn, e := t.kernel.RealtimeDial(ctx, t.x, p, dialURL)
	if e != nil {
		client.Close(websocket.StatusInternalError, "upstream unavailable")
		t.x.Failure = e
		if t.x.Strict() {
			t.kernel.Finish(t.x, &kernel.Outcome{Err: e, Committed: true}, terminalStatus)
		} else {
			t.kernel.Finish(t.x, nil, e.Status)
		}
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	limit := t.kernel.Config.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	client.SetReadLimit(bodylimit.Lower(limit, route.MaxBodyBytes))
	conn.SetReadLimit(limit)
	t.x.MarkDispatched()
	usage, observed, relayErr := t.surface.relayRealtime(ctx, t.x, p, client, conn, t.token, t.authority.ID)
	t.finishRelay(terminalStatus, usage, observed, relayErr)
}

func (t sessionTurn) finishRelay(terminalStatus int, observedUsage *wire.Usage, observed bool, relayErr error) {
	x := t.x
	class := kernel.ClassProtocol
	cancelled := false
	var ineligible *realtimeCredentialError
	if relayErr != nil {
		switch {
		case errors.Is(relayErr, errRealtimeAuthorityRevoked), errors.As(relayErr, &ineligible):
			class = kernel.ClassCredential
		case errors.Is(relayErr, errRealtimeClientClosed):
			class, cancelled = kernel.ClassCancelled, true
		case errors.Is(relayErr, context.Canceled):
			class, cancelled = kernel.ClassCancelled, true
		case errors.Is(relayErr, context.DeadlineExceeded):
			class = kernel.ClassTimeout
		}
	}
	if fact := x.LastAttemptFact(); fact != nil {
		if fact.Interaction != nil {
			if observed {
				fact.Interaction.ClientState = usage.ClientPartial
			}
			if relayErr == nil {
				fact.Interaction.UpstreamState = upstream.Terminal
				fact.Interaction.ClientState = usage.ClientTerminal
			} else {
				fact.Interaction.UpstreamState = upstream.Unknown
			}
		}
		if observedUsage != nil {
			fact.Usage = observedUsage
			fact.UsageObserved, fact.UsageComplete, fact.BillingUncertain = true, relayErr == nil, relayErr != nil
		} else {
			fact.UsageComplete = false
			fact.BillingUncertain = true
		}
		if relayErr != nil {
			fact.Class = class
		}
	}
	if relayErr == nil {
		t.kernel.Finish(x, &kernel.Outcome{Committed: true}, terminalStatus)
		return
	}
	x.Failure = fault.ServerError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract.")
	switch {
	case errors.Is(relayErr, errRealtimeAuthorityRevoked):
		x.Failure = fault.PermissionError("key_revoked", "The realtime session no longer has route authority.")
	case errors.As(relayErr, &ineligible):
		x.Failure = fault.PermissionError("provider_credential_"+string(ineligible.eligibility), "The realtime provider credential is no longer eligible.")
	case class == kernel.ClassCancelled:
		x.Failure = kernel.ClassFailure(kernel.ClassCancelled)
	case class == kernel.ClassTimeout:
		x.Failure = kernel.ClassFailure(kernel.ClassTimeout)
	}
	status := x.Failure.Status
	if x.Strict() {
		status = terminalStatus
	}
	t.kernel.Finish(x, &kernel.Outcome{Err: x.Failure, Committed: true, Cancelled: cancelled}, status)
}

func strictRealtimeHandshake(snapshot *routing.View, route *routing.Route, r *http.Request) *fault.Error {
	for _, target := range route.Targets {
		template, ok := snapshot.RealtimeTemplate(route.Slug, target.ID)
		if !ok {
			continue
		}
		if err := template.AdmitHandshake(r.URL.RawQuery, r.Header, route.Slug); err != nil {
			var refusal *oif.Incompatibility
			if errors.As(err, &refusal) {
				parameter := refusal.Field
				return fault.InvalidRequest(refusal.Code, refusal.Message, &parameter)
			}
			return fault.InvalidRequest("realtime_control_unavailable", "The realtime handshake is not qualified by this strict route.", nil)
		}
		return nil
	}
	return kernel.PolicyUnavailable("realtime_contract_unavailable", "The route has no compiled strict OpenAI realtime target.")
}

func realtimeURL(p *kernel.Pin) (string, *fault.Error) {
	endpoint, err := p.Provider.Connector().RealtimeURL(p.Model)
	if err != nil {
		return "", fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider profile cannot address this realtime model.")
	}
	return endpoint, nil
}
