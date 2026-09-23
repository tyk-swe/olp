package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/realtimecontract"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

const realtimeSession = time.Hour

const realtimeReauth = 5 * time.Second

const realtimePing = 30 * time.Second

var errRealtimeAuthorityRevoked = errors.New("realtime authority revoked")
var errRealtimeProviderCredentialRevoked = errors.New("realtime provider credential revoked")
var errRealtimeClientClosed = errors.New("realtime client disconnected")
var errRealtimeResponseIncomplete = errors.New("realtime response ended before its terminal event")

// The wire relay does not buffer native frames. This small state machine only
// records whether a response is still owed when either peer closes normally.
// A provider-controlled ID set is bounded; an untrackable event conservatively
// makes a clean socket close incomplete instead of claiming terminal delivery.
const maxRealtimePendingResponses = 128
const maxRealtimeTrackedFrameBytes = 1 << 20

type realtimeResponseState struct {
	requested int
	active    map[string]struct{}
	anonymous bool
	unknown   bool
}

func (s *realtimeResponseState) pending() bool {
	return s.requested > 0 || len(s.active) > 0 || s.anonymous || s.unknown
}

func (s *realtimeResponseState) add(id string) {
	if id == "" || len(id) > 512 {
		s.unknown = true
		return
	}
	if s.active == nil {
		s.active = make(map[string]struct{})
	}
	if _, found := s.active[id]; found {
		return
	}
	if len(s.active) >= maxRealtimePendingResponses {
		s.unknown = true
		return
	}
	s.active[id] = struct{}{}
}

func (s *realtimeResponseState) clientFrame(typ websocket.MessageType, data []byte) {
	if typ != websocket.MessageText || len(data) > maxRealtimeTrackedFrameBytes {
		s.unknown = true
		return
	}
	var event struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &event) != nil {
		s.unknown = true
		return
	}
	if event.Type == "response.create" || event.Type == "input_audio_buffer.commit" {
		if s.requested == maxRealtimePendingResponses {
			s.unknown = true
		} else {
			s.requested++
		}
	}
}

type realtimeFrame struct {
	Type       string `json:"type"`
	ResponseID string `json:"response_id"`
	Response   *struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			InputDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"input_token_details"`
		} `json:"usage"`
	} `json:"response"`
}

func (s *realtimeResponseState) providerFrame(typ websocket.MessageType, data []byte) *openai.Usage {
	if typ != websocket.MessageText || len(data) > maxRealtimeTrackedFrameBytes {
		s.unknown = true
		return nil
	}
	var event realtimeFrame
	if json.Unmarshal(data, &event) != nil {
		s.unknown = true
		return nil
	}
	id := event.ResponseID
	if event.Response != nil && event.Response.ID != "" {
		id = event.Response.ID
	}
	switch event.Type {
	case "response.created":
		if s.requested > 0 {
			s.requested--
		}
		s.add(id)
	case "response.done":
		if s.anonymous && len(s.active) > 0 {
			// Without a response ID on a preceding fragment, this terminal
			// cannot prove which concurrent response that fragment belonged to.
			s.unknown = true
		}
		if id != "" {
			if _, found := s.active[id]; found {
				delete(s.active, id)
			} else if s.requested > 0 {
				s.requested--
			} else if len(s.active) > 0 {
				s.unknown = true
			}
		} else if len(s.active) > 0 {
			s.unknown = true
		} else if s.requested > 0 {
			s.requested--
		}
		// Some native response fragments do not identify their response.
		// A terminal response closes that observation, while an unparseable or
		// unmatched event remains uncertain for the rest of the session.
		s.anonymous = false
	default:
		if strings.HasPrefix(event.Type, "response.") {
			if id != "" {
				if _, found := s.active[id]; !found && s.requested > 0 {
					s.requested--
				}
				s.add(id)
			} else {
				s.anonymous = true
			}
		}
	}
	if event.Type != "response.done" || event.Response == nil || event.Response.Usage == nil {
		return nil
	}
	u := event.Response.Usage
	if u.InputTokens < 0 || u.OutputTokens < 0 {
		return nil
	}
	out := &openai.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
	if u.InputDetails != nil && u.InputDetails.CachedTokens != nil {
		cached := *u.InputDetails.CachedTokens
		if cached < 0 || cached > u.InputTokens {
			return nil
		}
		out.CachedInputTokens = &cached
	}
	return out
}

func (s *Server) realtime(w http.ResponseWriter, r *http.Request) {
	x := &execution{request: s.begin(w, r), family: openai.FamilyRealtime, actor: "api_key"}
	x.mode = "realtime"
	fail := func(e *Error) {
		x.failure = e
		s.finish(x, nil, e.Status)
		writeSurfaceError(w, e, "openai")
	}
	if !s.admit(r.Context()) {
		fail(overloaded)
		return
	}
	defer s.release(r.Context())

	if origin := r.Header.Get("Origin"); origin != "" && !slices.Contains(s.cfg.CORSAllowedOrigins, origin) {
		fail(permissionError("origin_forbidden", "This origin is not allowed to open realtime sessions."))
		return
	}

	if !realtimeUpgrade(r) {
		fail(invalidRequest("websocket_upgrade_required", "This endpoint requires a WebSocket upgrade request.", nil))
		return
	}
	token := bearerToken(r)
	if token == "" {
		fail(authenticationError("missing_authorization", "Provide an Authorization bearer API key."))
		return
	}
	authority, e := s.authenticate(r, "inference")
	if e != nil {
		fail(e)
		return
	}
	x.keyID, x.affinity = authority.ID, []byte(authority.ID)
	x.budgetGroupID = authority.BudgetGroupID
	attributionValues := r.Header.Values(usage.AttributionHeader)
	if len(attributionValues) == 0 {
		attributionValues = r.URL.Query()["attribution"]
	}
	if x.attribution, e = parseAttributionValues(attributionValues, authority); e != nil {
		fail(e)
		return
	}
	slug := r.URL.Query().Get("model")
	if !openai.RouteSlug.MatchString(slug) {
		fail(invalidRequest("invalid_value", "model must name a published route slug.", strPtr("model")))
		return
	}
	snapshot := x.request.release.Snapshot
	route, ok := snapshot.Routes[slug]
	if !ok {
		fail(modelNotFound(slug))
		return
	}
	x.route = &route
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		fail(permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."))
		return
	}
	if !slices.Contains(route.Operations, "realtime") {
		fail(invalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow realtime sessions.", nil))
		return
	}
	if e := policySurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	if x.strict() {
		if e := strictRealtimeHandshake(snapshot, &route, r); e != nil {
			fail(e)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), realtimeSession)
	defer cancel()
	var p *pin
	if x.strict() {
		// Realtime has an operation-owned duplex contract. Retained Responses
		// eligibility and the unary codec say nothing about a native session.
		p, e = s.selectPinSurface(ctx, x, &route, "realtime", "openai", "realtime", func(provider *runtime.Provider, model string) bool {
			return provider.Connector().Supports("realtime", "openai", "realtime") && provider.Supports(model, "realtime", "openai", "realtime")
		})
	} else {
		p, e = s.selectPin(ctx, x, &route, "realtime", "realtime")
	}
	if e != nil {
		fail(e)
		return
	}
	defer s.resourceSettle(r.Context(), x, p)
	if x.strict() {
		if _, ok := snapshot.RealtimeTemplate(route.Slug, p.target.ID); !ok {
			fail(policyUnavailable("realtime_contract_unavailable", "The selected target has no compiled strict realtime contract."))
			return
		}
	}
	if e := s.reserveState(ctx, x, authority, realtimeSession); e != nil {
		fail(e)
		return
	}
	upstream, e := realtimeURL(p)
	if e != nil {
		fail(e)
		return
	}
	check := strings.Replace(upstream, "wss://", "https://", 1)
	check = strings.Replace(check, "ws://", "http://", 1)
	if u, e := url.Parse(check); e == nil {
		u.RawQuery, u.Fragment = "", ""
		check = u.String()
	}
	if _, err := s.egress.ValidateEndpoint(check); err != nil {
		fail(serverError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy."))
		return
	}

	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		fail(serverError(http.StatusBadRequest, "websocket_upgrade_failed", "The WebSocket upgrade could not be completed."))
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	terminalStatus := http.StatusOK
	if x.strict() {
		// A strict WebSocket has already committed HTTP 101 at this boundary.
		// Later transport errors are carried by close frames and the Attempt.
		terminalStatus = http.StatusSwitchingProtocols
	}
	conn, e := realtimeDial(ctx, s, x, p, upstream)
	if e != nil {

		client.Close(websocket.StatusInternalError, "upstream unavailable")
		x.failure = e
		if x.strict() {
			s.finish(x, &outcome{err: e, committed: true}, terminalStatus)
		} else {
			s.finish(x, nil, e.Status)
		}
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	limit := s.cfg.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	client.SetReadLimit(limit)
	conn.SetReadLimit(limit)
	x.dispatched = true
	x.delivered(s.now())
	observedUsage, observed, relayErr := s.relayRealtime(ctx, x, p, client, conn, token, authority.ID)
	class := classProtocol
	cancelled := false
	if relayErr != nil {
		switch {
		case errors.Is(relayErr, errRealtimeAuthorityRevoked), errors.Is(relayErr, errRealtimeProviderCredentialRevoked):
			class = classCredential
		case errors.Is(relayErr, errRealtimeClientClosed):
			class, cancelled = classCancelled, true
		case errors.Is(relayErr, context.Canceled):
			class, cancelled = classCancelled, true
		case errors.Is(relayErr, context.DeadlineExceeded):
			class = classTimeout
		}
	}
	if len(x.facts) > 0 {
		fact := &x.facts[len(x.facts)-1]
		if fact.Interaction != nil {
			if observed {
				fact.Interaction.ClientState = usage.ClientPartial
			}
			if relayErr == nil {
				fact.Interaction.UpstreamState = usage.UpstreamTerminal
				fact.Interaction.ClientState = usage.ClientTerminal
			} else {
				fact.Interaction.UpstreamState = usage.UpstreamUnknown
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
		x.failure = serverError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract.")
		switch {
		case errors.Is(relayErr, errRealtimeAuthorityRevoked):
			x.failure = permissionError("key_revoked", "The realtime session no longer has route authority.")
		case errors.Is(relayErr, errRealtimeProviderCredentialRevoked):
			x.failure = permissionError("provider_credential_revoked", "The realtime provider credential was revoked.")
		case class == classCancelled:
			x.failure = (&attemptFailure{class: classCancelled}).toError()
		case class == classTimeout:
			x.failure = (&attemptFailure{class: classTimeout}).toError()
		}
		status := x.failure.Status
		if x.strict() {
			status = terminalStatus
		}
		s.finish(x, &outcome{err: x.failure, committed: true, cancelled: cancelled}, status)
		return
	}
	s.finish(x, &outcome{committed: true}, terminalStatus)
}

func strictRealtimeHandshake(snapshot *runtime.Snapshot, route *runtime.Route, r *http.Request) *Error {
	for _, target := range route.Targets {
		template, ok := snapshot.RealtimeTemplate(route.Slug, target.ID)
		if !ok {
			continue
		}
		if err := template.AdmitHandshake(r.URL.RawQuery, r.Header, route.Slug); err != nil {
			var refusal *realtimecontract.Refusal
			if errors.As(err, &refusal) {
				parameter := refusal.Field
				return invalidRequest("realtime_control_unavailable", refusal.Message, &parameter)
			}
			return invalidRequest("realtime_control_unavailable", "The realtime handshake is not qualified by this strict route.", nil)
		}
		return nil
	}
	return policyUnavailable("realtime_contract_unavailable", "The route has no compiled strict OpenAI realtime target.")
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

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if len(header) < 7 || !strings.EqualFold(header[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(header[7:])
}

func realtimeURL(p *pin) (string, *Error) {
	endpoint, err := p.provider.Connector().RealtimeURL(p.model)
	if err != nil {
		return "", serverError(http.StatusBadGateway, "upstream_error", "The provider profile cannot address this realtime model.")
	}
	return endpoint, nil
}

func realtimeDial(ctx context.Context, s *Server, x *execution, p *pin, endpoint string) (*websocket.Conn, *Error) {
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1)
	fact.Mode = "realtime"
	finish := func(class string, e *Error) *Error {
		fact.Class = class
		if fact.Interaction != nil && fact.Status > 0 {
			fact.Interaction.UpstreamState = usage.UpstreamTerminal
		}
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(true)
		x.facts = append(x.facts, fact)
		return e
	}
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, finish(classConnect, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved."))
	}
	var secret []byte
	if p.slot.CredentialID != nil {
		secret, _ = x.request.release.Credential(*p.slot.CredentialID)
	}
	if _, err := s.auth.Apply(ctx, probe, p.provider.Connector(), secret, nil); err != nil {
		return nil, finish(classCredential, serverError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied."))
	}
	headers := http.Header{}
	for name, values := range probe.Header {
		headers[name] = append([]string{}, values...)
	}
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		return nil, finish(classCredential, serverError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable."))
	}
	conn, resp, err := websocket.Dial(ctx, probe.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
	if err != nil {
		if fact.Interaction != nil {
			fact.Interaction.UpstreamState = usage.UpstreamUnknown
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		fact.Status = status
		class := classConnect
		switch {
		case ctx.Err() != nil:
			class = classCancelled
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			class = classCredential
		case status == http.StatusTooManyRequests:
			class = classRateLimit
		case status >= 500:
			class = classUpstreamServer
		case status != 0:
			class = classUpstreamClient
		}
		return nil, finish(class, upstreamError(&attemptFailure{status: status, upstream: upstreamResponseError(resp)}))
	}
	fact.Class = "success"
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamAccepted
	}
	fact.Committed = true
	fact.Duration = s.now().Sub(fact.StartedAt)
	fact.recordEvidence(true)
	x.facts = append(x.facts, fact)
	return conn, nil
}

func upstreamResponseError(resp *http.Response) *openai.UpstreamError {
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

func (s *Server) relayRealtime(ctx context.Context, x *execution, p *pin, client, conn *websocket.Conn, token, keyID string) (*openai.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var usageMu sync.Mutex
	usage := &openai.Usage{}
	have := false
	observed := false
	var responses realtimeResponseState
	type relayEnd struct {
		err            error
		client         bool
		deliveryFailed bool
	}
	forward := func(dst, src *websocket.Conn, inspect bool) relayEnd {
		for {
			typ, data, err := src.Read(ctx)
			if err != nil {
				return relayEnd{err: err, client: !inspect}
			}
			if inspect {
				usageMu.Lock()
				if u := responses.providerFrame(typ, data); u != nil {
					usage.InputTokens = addBounded(usage.InputTokens, u.InputTokens)
					usage.OutputTokens = addBounded(usage.OutputTokens, u.OutputTokens)
					if u.CachedInputTokens != nil {
						cached := *u.CachedInputTokens
						if usage.CachedInputTokens != nil {
							cached = addBounded(cached, *usage.CachedInputTokens)
						}
						usage.CachedInputTokens = &cached
					}
					have = true
				}
				usageMu.Unlock()
			} else if x.strict() {
				usageMu.Lock()
				responses.clientFrame(typ, data)
				usageMu.Unlock()
			}
			writeCtx, stopWrite := context.WithTimeout(ctx, responseWriteTimeout)
			err = dst.Write(writeCtx, typ, data)
			stopWrite()
			if err != nil {
				return relayEnd{err: err, client: inspect, deliveryFailed: inspect}
			}
			if inspect {
				observed = true
			}
		}
	}
	done := make(chan relayEnd, 2)
	go func() { done <- forward(conn, client, false) }()
	go func() { done <- forward(client, conn, true) }()
	reauth := time.NewTicker(realtimeReauth)
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
			first, clientClosed, clientDeliveryFailed = end.err, end.client, end.deliveryFailed
			break loop
		case <-reauth.C:
			authority, err := s.Runtime.Authenticate(token)
			if err != nil || authority.ID != keyID || !authority.Allows("inference", x.route.Slug, x.route.ProjectID, s.now()) {
				first = errRealtimeAuthorityRevoked
				closeCode = websocket.StatusPolicyViolation
				client.Close(websocket.StatusPolicyViolation, "key revoked")
				break loop
			}
			if p.slot.CredentialID != nil && s.Runtime.Revoked(*p.slot.CredentialID) {
				first = errRealtimeProviderCredentialRevoked
				closeCode = websocket.StatusPolicyViolation
				client.Close(websocket.StatusPolicyViolation, "provider credential revoked")
				break loop
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, realtimeReauth)
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
	pending := x.strict() && responses.pending()
	usageMu.Unlock()
	if !clientClosed && pending && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		// The provider closed a socket while a native response still owed
		// its response.done terminal event. Never mirror its 1000/1001 as
		// a successful client-visible session end.
		code = websocket.StatusInternalError
	}
	conn.Close(code, "")
	client.Close(code, "")
	second := <-done
	clientDeliveryFailed = clientDeliveryFailed || second.deliveryFailed
	usageMu.Lock()
	defer usageMu.Unlock()
	var observedUsage *openai.Usage
	if have {
		observedUsage = usage
	}
	pending = x.strict() && responses.pending()
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

func strPtr(value string) *string { return &value }
