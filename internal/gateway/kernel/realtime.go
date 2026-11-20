package kernel

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/gateway/realtimeresponses"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/usage"
)

const realtimeSession = time.Hour

const realtimePing = 30 * time.Second

const webRTCBound = time.Hour

var errRealtimeAuthorityRevoked = errors.New("realtime authority revoked")
var errRealtimeClientClosed = errors.New("realtime client disconnected")
var errRealtimeResponseIncomplete = errors.New("realtime response ended before its terminal event")

// realtimeCredentialError ends a realtime session whose provider credential
// is no longer eligible; the session's failure names why.
type realtimeCredentialError struct{ eligibility routing.Eligibility }

func (e *realtimeCredentialError) Error() string {
	return "realtime provider credential " + string(e.eligibility)
}

func (s *Server) realtime(w http.ResponseWriter, r *http.Request) {
	x := &Call{request: s.begin(w, r), family: wire.Realtime, actor: "api_key"}
	x.mode = "realtime"
	fail := func(e *fault.Error) { s.Refuse(x, w, e) }
	if !s.Gate.Admit(r.Context()) {
		fail(fault.Overloaded)
		return
	}
	defer s.Gate.Release(r.Context())

	if !s.browserOriginAllowed(r) {
		fail(fault.PermissionError("origin_forbidden", "This origin is not allowed to open realtime sessions."))
		return
	}

	if !realtimeUpgrade(r) {
		fail(fault.InvalidRequest("websocket_upgrade_required", "This endpoint requires a WebSocket upgrade request.", nil))
		return
	}
	token := ingress.BearerToken(r)
	if token == "" {
		fail(fault.AuthenticationError("missing_authorization", "Provide an Authorization bearer API key."))
		return
	}
	authority, e := s.Gate.AuthenticateRequest(r, token, "inference")
	if e != nil {
		fail(e)
		return
	}
	s.bindAuthority(x, authority)
	attributionValues := r.Header.Values(attribution.Header)
	if len(attributionValues) == 0 {
		attributionValues = r.URL.Query()["attribution"]
	}
	if x.attribution, e = ingress.ParseAttributionValues(attributionValues, authority); e != nil {
		fail(e)
		return
	}
	slug := r.URL.Query().Get("model")
	if !routeidentity.ValidSlug(slug) {
		fail(fault.InvalidRequest("invalid_value", "model must name a published route slug.", new("model")))
		return
	}
	snapshot := x.request.release.Snapshot()
	route, ok := snapshot.FindRoute(slug)
	if !ok {
		fail(fault.ModelNotFound(slug))
		return
	}
	x.Route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
		fail(fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."))
		return
	}
	if !slices.Contains(route.Operations, "realtime") {
		fail(fault.InvalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow realtime sessions.", nil))
		return
	}
	if e := x.GuardrailSurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	if x.Strict() {
		if e := strictRealtimeHandshake(snapshot, &route, r); e != nil {
			fail(e)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), realtimeSession)
	defer cancel()
	x.estimate = resourceEstimate
	var p *Pin
	defer func() { s.settleSession(r.Context(), x, p) }()
	if x.Strict() {
		// Realtime has an operation-owned duplex contract. Retained Responses
		// eligibility and the unary codec say nothing about a native session.
		p, e = s.SelectPinSurface(ctx, x, &route, "realtime", "openai", "realtime", func(provider *routing.Provider, model string) bool {
			return provider.Connector().Supports("realtime", "openai", "realtime") && provider.Supports(model, "realtime", "openai", "realtime")
		})
	} else {
		p, e = s.selectPin(ctx, x, &route, "realtime", "realtime")
	}
	if e != nil {
		fail(e)
		return
	}
	if x.Strict() {
		if _, ok := snapshot.RealtimeTemplate(route.Slug, p.Target.ID); !ok {
			fail(PolicyUnavailable("realtime_contract_unavailable", "The selected target has no compiled strict realtime contract."))
			return
		}
	}
	if e := s.reserveState(ctx, x, authority, realtimeSession); e != nil {
		fail(e)
		return
	}
	if e := s.reserveSessionCost(ctx, x, authority, p.attempt, realtimeSession); e != nil {
		fail(e)
		return
	}
	dialURL, e := realtimeURL(p)
	if e != nil {
		fail(e)
		return
	}
	check := strings.Replace(dialURL, "wss://", "https://", 1)
	check = strings.Replace(check, "ws://", "http://", 1)
	if u, e := url.Parse(check); e == nil {
		u.RawQuery, u.Fragment = "", ""
		check = u.String()
	}
	if _, err := s.Egress.ValidateEndpoint(check); err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy."))
		return
	}

	// The upgrade carries no allowance or metadata headers. The provider is not
	// dialled until the session is accepted, so there is no attempt to describe,
	// and a WebSocket client does not read the rate-limit headers of an HTTP API.
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		fail(fault.ServerError(http.StatusBadRequest, "websocket_upgrade_failed", "The WebSocket upgrade could not be completed."))
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	terminalStatus := http.StatusOK
	if x.Strict() {
		// A strict WebSocket has already committed HTTP 101 at this boundary.
		// Later transport errors are carried by close frames and the Attempt.
		terminalStatus = http.StatusSwitchingProtocols
	}
	conn, e := realtimeDial(ctx, s, x, p, dialURL)
	if e != nil {

		client.Close(websocket.StatusInternalError, "upstream unavailable")
		x.Failure = e
		if x.Strict() {
			s.Finish(x, &Outcome{Err: e, Committed: true}, terminalStatus)
		} else {
			s.Finish(x, nil, e.Status)
		}
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	limit := s.Config.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	client.SetReadLimit(bodylimit.Lower(limit, route.MaxBodyBytes))
	conn.SetReadLimit(limit)
	x.dispatched = true
	observedUsage, observed, relayErr := s.relayRealtime(ctx, x, p, client, conn, token, authority.ID)
	class := ClassProtocol
	cancelled := false
	var ineligible *realtimeCredentialError
	if relayErr != nil {
		switch {
		case errors.Is(relayErr, errRealtimeAuthorityRevoked), errors.As(relayErr, &ineligible):
			class = classCredential
		case errors.Is(relayErr, errRealtimeClientClosed):
			class, cancelled = ClassCancelled, true
		case errors.Is(relayErr, context.Canceled):
			class, cancelled = ClassCancelled, true
		case errors.Is(relayErr, context.DeadlineExceeded):
			class = ClassTimeout
		}
	}
	if len(x.facts) > 0 {
		fact := &x.facts[len(x.facts)-1]
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
	if relayErr != nil {
		x.Failure = fault.ServerError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract.")
		switch {
		case errors.Is(relayErr, errRealtimeAuthorityRevoked):
			x.Failure = fault.PermissionError("key_revoked", "The realtime session no longer has route authority.")
		case errors.As(relayErr, &ineligible):
			x.Failure = fault.PermissionError("provider_credential_"+string(ineligible.eligibility), "The realtime provider credential is no longer eligible.")
		case class == ClassCancelled:
			x.Failure = (&AttemptFailure{class: ClassCancelled}).Fault()
		case class == ClassTimeout:
			x.Failure = (&AttemptFailure{class: ClassTimeout}).Fault()
		}
		status := x.Failure.Status
		if x.Strict() {
			status = terminalStatus
		}
		s.Finish(x, &Outcome{Err: x.Failure, Committed: true, Cancelled: cancelled}, status)
		return
	}
	s.Finish(x, &Outcome{Committed: true}, terminalStatus)
}

func strictRealtimeHandshake(snapshot *routing.View, route *routing.Route, r *http.Request) *fault.Error {
	for _, target := range route.Targets {
		template, ok := snapshot.RealtimeTemplate(route.Slug, target.ID)
		if !ok {
			continue
		}
		if err := template.AdmitHandshake(r.URL.RawQuery, r.Header, route.Slug); err != nil {
			if refusal, ok := errors.AsType[*oif.Incompatibility](err); ok {
				parameter := refusal.Field
				return fault.InvalidRequest(refusal.Code, refusal.Message, &parameter)
			}
			return fault.InvalidRequest("realtime_control_unavailable", "The realtime handshake is not qualified by this strict route.", nil)
		}
		return nil
	}
	return PolicyUnavailable("realtime_contract_unavailable", "The route has no compiled strict OpenAI realtime target.")
}

func realtimeUpgrade(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	upgrades := false
	for _, line := range r.Header.Values("Connection") {
		for token := range strings.SplitSeq(line, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				upgrades = true
			}
		}
	}
	if !upgrades || !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") {
		return false
	}
	if strings.TrimSpace(r.Header.Get("Sec-WebSocket-Version")) != "13" {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key")))
	return err == nil && len(key) == 16
}

func realtimeURL(p *Pin) (string, *fault.Error) {
	endpoint, err := p.Provider.Connector().RealtimeURL(p.Model)
	if err != nil {
		return "", fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider profile cannot address this realtime model.")
	}
	return endpoint, nil
}

func realtimeDial(ctx context.Context, s *Server, x *Call, p *Pin, endpoint string) (*websocket.Conn, *fault.Error) {
	fact := s.newFact(x, p.attempt, p.Slot, len(x.facts)+1, &p.Provider)
	fact.Mode = "realtime"
	state := attemptState{parent: ctx, classifier: upstream.Classifier{Declared: p.Provider.Connector().Classification()}}
	ctx = httptrace.WithClientTrace(ctx, state.trace())
	if p.Hold != nil {
		fact.Budgets = p.Hold.budgets
	}
	finish := func(class string, e *fault.Error) *fault.Error {
		fact.Class = class
		fact.Duration = s.Now().Sub(fact.StartedAt)
		x.dispatched = x.dispatched || state.dispatched.Load()
		fact.RecordEvidence(state.evidence().Acceptance().Unresolved())
		x.facts = append(x.facts, fact)
		return e
	}
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, finish(ClassConnect, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."))
	}
	if err := s.applySlotCredential(ctx, x, probe, p.Provider.Connector(), p.Slot, nil, &p.Provider, p.Model); err != nil {
		return nil, finish(classCredential, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied."))
	}
	headers := http.Header{}
	for name, values := range probe.Header {
		headers[name] = append([]string{}, values...)
	}
	client, err := s.providerClient(ctx, x.request.release, &p.Provider, p.Slot)
	if err != nil {
		return nil, finish(classCredential, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable."))
	}
	conn, resp, err := websocket.Dial(ctx, probe.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
	if resp != nil {
		state.dispatched.Store(true)
	}
	x.dispatched = x.dispatched || state.dispatched.Load()
	if state.dispatched.Load() && p.Hold != nil {
		x.spendCaps(p.Hold.budgets, p.attempt)
	}
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		fact.Status = status
		state.status = status
		nativeError := x.redacted(upstreamResponseError(resp))
		evidence := state.evidence()
		evidence.Error, evidence.Interrupted, evidence.Err = nativeError, ctx.Err(), err
		outcome := state.classifier.Classify(evidence)
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = outcome.Acceptance
		}
		return nil, finish(string(outcome.Class), UpstreamError(&AttemptFailure{Status: status, upstream: nativeError}))
	}
	fact.Class = "success"
	fact.Status = http.StatusOK
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = upstream.Accepted
	}
	fact.Committed = true
	fact.Duration = s.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(true)
	x.facts = append(x.facts, fact)
	return conn, nil
}

func upstreamResponseError(resp *http.Response) *wire.UpstreamError {
	if resp == nil || resp.Body == nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	if err != nil {
		return nil
	}
	return openai.ParseErrorBody(body)
}

// redactRealtimeError scrubs applied credential values from a provider error
// frame before it reaches the client. Any other frame returns unchanged, so
// the relay stays byte-for-byte for everything it does not recognize. A frame
// that does not parse at all still gets a raw byte-level scrub: an error
// document too malformed to decode must not carry a credential through.
func redactRealtimeError(data []byte, credentials egress.Sensitive) ([]byte, error) {
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: len(data) + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return []byte(credentials.Redact(string(data))), nil
	}
	isError := false
	if _, ok := doc.Root().Lookup("error"); ok {
		isError = true
	} else if kind, ok := doc.Root().Lookup("type"); ok {
		text, _ := kind.Text()
		isError = text == "error"
	}
	if !isError {
		return data, nil
	}
	redacted, err := fault.RedactNativeFailureDocument(doc, credentials)
	if err != nil {
		return nil, err
	}
	return redacted.Bytes(), nil
}

// realtimeBoundedWriter keeps one watchdog per direction. Every frame gets a
// fresh full write window without a new context timer for each frame. The
// WebSocket's own cancellation hook still closes a stalled connection and
// interrupts a writer waiting on its internal lock.
type realtimeFrameSink interface {
	Write(context.Context, websocket.MessageType, []byte) error
}

type realtimeBoundedWriter struct {
	dst     realtimeFrameSink
	parent  context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	expired atomic.Bool
}

func newRealtimeBoundedWriter(parent context.Context, dst realtimeFrameSink, timeout time.Duration) *realtimeBoundedWriter {
	w := &realtimeBoundedWriter{dst: dst, parent: parent, timeout: timeout}
	w.ctx, w.cancel = context.WithCancel(parent)
	w.timer = time.AfterFunc(timeout, func() {
		w.expired.Store(true)
		w.cancel()
	})
	w.timer.Stop()
	return w
}

func (w *realtimeBoundedWriter) close() {
	w.timer.Stop()
	w.cancel()
}

func (w *realtimeBoundedWriter) write(typ websocket.MessageType, data []byte) error {
	if err := w.parent.Err(); err != nil {
		return err
	}
	w.timer.Reset(w.timeout)
	err := w.dst.Write(w.ctx, typ, data)
	if !w.timer.Stop() {
		// The deadline won a race with successful completion. Conservatively
		// terminate this direction; it cannot start another bounded write.
		w.expired.Store(true)
		w.cancel()
	}
	if w.expired.Load() {
		return context.DeadlineExceeded
	}
	if parentErr := w.parent.Err(); parentErr != nil {
		return parentErr
	}
	return err
}

func (s *Server) relayRealtime(ctx context.Context, x *Call, p *Pin, client, conn duplexPeer, token, keyID string) (*wire.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var usageMu sync.Mutex
	usage := &wire.Usage{}
	have := false
	observed := false
	var responses realtimeresponses.Tracker
	type relayEnd struct {
		err            error
		client         bool
		deliveryFailed bool
	}
	forward := func(dst, src duplexPeer, inspect bool) relayEnd {
		writer := newRealtimeBoundedWriter(ctx, dst, fault.ResponseWriteTimeout)
		defer writer.close()
		for {
			typ, data, err := src.Read(ctx)
			if err != nil {
				return relayEnd{err: err, client: !inspect}
			}
			if inspect {
				func() {
					usageMu.Lock()
					defer usageMu.Unlock()
					if u := responses.ProviderFrame(typ, data, x.Strict()); u != nil {
						usage.InputTokens = quota.AddBounded(usage.InputTokens, u.InputTokens)
						usage.OutputTokens = quota.AddBounded(usage.OutputTokens, u.OutputTokens)
						addUsageShare(&usage.CachedInputTokens, u.CachedInputTokens)
						addUsageShare(&usage.AudioInputTokens, u.AudioInputTokens)
						addUsageShare(&usage.CachedAudioInputTokens, u.CachedAudioInputTokens)
						addUsageShare(&usage.AudioOutputTokens, u.AudioOutputTokens)
						addUsageShare(&usage.ImageInputTokens, u.ImageInputTokens)
						addUsageShare(&usage.CachedImageInputTokens, u.CachedImageInputTokens)
						have = true
					}
				}()
			} else if x.Strict() {
				func() {
					usageMu.Lock()
					defer usageMu.Unlock()
					responses.ClientFrame(typ, data)
				}()
			}
			if inspect && typ == websocket.MessageText &&
				(bytes.Contains(data, []byte(`"error"`)) || bytes.Contains(data, []byte(`\`))) {
				// An in-band provider error can echo an applied credential;
				// scrub it before the frame reaches the client. The gate also
				// admits any frame carrying a JSON escape, since a member or
				// value spelled like "err\u006fr" hides the marker.
				scrubbed, err := redactRealtimeError(data, x.sensitive)
				if err != nil {
					return relayEnd{err: err}
				}
				data = scrubbed
			}
			err = writer.write(typ, data)
			if err != nil {
				return relayEnd{err: err, client: inspect, deliveryFailed: inspect}
			}
			if inspect {
				x.Delivered(s.Now())
				observed = true
			}
		}
	}
	done := make(chan relayEnd, 2)
	remaining := 2
	relay := func(dst, src duplexPeer, inspect bool) {
		defer panics.Recover(s.Log, "realtime relay", func(err error) { done <- relayEnd{err: err} })
		done <- forward(dst, src, inspect)
	}
	go relay(conn, client, false)
	go relay(client, conn, true)
	reauth := time.NewTicker(ingress.ReauthInterval)
	defer reauth.Stop()
	heartbeat := time.NewTicker(realtimePing)
	defer heartbeat.Stop()
	var first error
	clientClosed := false
	clientDeliveryFailed := false
	closeCode := websocket.StatusCode(0)
loop:
	for {
		select {
		case end := <-done:
			remaining--
			first, clientClosed, clientDeliveryFailed = end.err, end.client, end.deliveryFailed
			break loop
		case <-reauth.C:
			authority, err := s.reauthenticateSession(ctx, x, token)
			if err != nil || authority.ID != keyID || !authority.Allows("inference", x.Route.Slug, x.Route.ProjectID, s.Now()) || !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) {
				first = errRealtimeAuthorityRevoked
				closeCode = websocket.StatusPolicyViolation
				client.Close(websocket.StatusPolicyViolation, "authority revoked")
				break loop
			}
			if eligibility := s.pinEligibility(p); eligibility != routing.Eligible {
				first = &realtimeCredentialError{eligibility}
				closeCode = websocket.StatusPolicyViolation
				client.Close(websocket.StatusPolicyViolation, "provider credential "+string(eligibility))
				break loop
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, ingress.ReauthInterval)
			err := conn.Ping(pingCtx)
			if err == nil {
				err = client.Ping(pingCtx)
			}
			pingCancel()
			if err != nil {
				first = err
				break loop
			}
		case <-ctx.Done():
			first = ctx.Err()
			break loop
		}
	}
	cancel()

	code := websocket.CloseStatus(first)
	if closeCode != 0 {
		code = closeCode
	} else if code < 0 {
		code = websocket.StatusInternalError
	}
	usageMu.Lock()
	pending := x.Strict() && responses.Pending()
	usageMu.Unlock()
	if !clientClosed && pending && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		// The provider closed a socket while a native response still owed
		// its response.done terminal event. Never mirror its 1000/1001 as
		// a successful client-visible session end.
		code = websocket.StatusInternalError
	}
	conn.Close(code, "")
	client.Close(code, "")
	// A control event has consumed neither result; a forwarding result has
	// consumed one. Join every remaining task before final usage/delivery state.
	for ; remaining > 0; remaining-- {
		end := <-done
		clientDeliveryFailed = clientDeliveryFailed || end.deliveryFailed
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	var observedUsage *wire.Usage
	if have {
		observedUsage = usage
	}
	pending = x.Strict() && responses.Pending()
	if clientClosed && (pending || clientDeliveryFailed) {
		return observedUsage, observed, fmt.Errorf("%w: %v", errRealtimeClientClosed, first)
	}
	if !clientClosed && pending && (websocket.CloseStatus(first) == websocket.StatusNormalClosure || websocket.CloseStatus(first) == websocket.StatusGoingAway) {
		return observedUsage, observed, fmt.Errorf("%w: %v", errRealtimeResponseIncomplete, first)
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		return observedUsage, observed, nil
	}
	if clientClosed {
		return observedUsage, observed, fmt.Errorf("%w: %v", errRealtimeClientClosed, first)
	}
	if !have {
		return nil, observed, first
	}
	return usage, observed, first
}
