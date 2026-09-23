package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/geminilifecycle"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

const geminiLiveSession = time.Hour
const geminiLiveFrameWriteTimeout = 5 * time.Second

var (
	errGeminiLiveProviderError  = errors.New("Gemini Live provider reported an error")
	errGeminiLiveIncomplete     = errors.New("Gemini Live provider closed with work in flight")
	errGeminiLiveClientClosed   = errors.New("Gemini Live client closed with work in flight")
	errGeminiLiveProtocol       = errors.New("Gemini Live provider protocol violation")
	errGeminiLiveClientProtocol = errors.New("Gemini Live client protocol violation")
	errGeminiLiveAuthority      = errors.New("Gemini Live authority revoked")
)

func (s *Server) registerGeminiLifecycle(mux *http.ServeMux) {
	live := connectors.GeminiLiveMethod
	mux.HandleFunc("GET /gemini/ws/"+live, s.geminiLive)
	mux.HandleFunc("GET /ws/"+live, s.geminiLive)
	mux.HandleFunc("OPTIONS /gemini/ws/", s.preflight)
	mux.HandleFunc("POST /gemini/v1beta/interactions", s.geminiInteractionCreate)
	mux.HandleFunc("GET /gemini/v1beta/interactions/{id}", s.geminiInteractionResource)
	mux.HandleFunc("DELETE /gemini/v1beta/interactions/{id}", s.geminiInteractionResource)
	mux.HandleFunc("POST /gemini/v1beta/interactions/{id}/cancel", s.geminiInteractionResource)
}

// geminiClientKey validates all SDK-supported key locations before the shared
// authority authenticates one value. Duplicate keys or competing locations
// must not silently select the first query/header value.
func geminiClientKey(r *http.Request, allowedQuery ...string) (string, *Error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", invalidRequest("invalid_request", "The Gemini query is malformed.", nil)
	}
	for name, values := range query {
		if !slices.Contains(append([]string{"key"}, allowedQuery...), name) || len(values) != 1 || values[0] == "" {
			return "", invalidRequest("invalid_request", "The Gemini query has an unsupported or repeated setting.", nil)
		}
	}
	forms := 0
	token := ""
	if values := r.Header.Values("Authorization"); len(values) > 0 {
		forms++
		if len(values) != 1 {
			return "", invalidRequest("invalid_request", "Provide one Gemini API key.", nil)
		}
		token = bearerToken(r)
	}
	if values := r.Header.Values("X-Goog-Api-Key"); len(values) > 0 {
		forms++
		if len(values) != 1 || values[0] == "" {
			return "", invalidRequest("invalid_request", "Provide one Gemini API key.", nil)
		}
		token = values[0]
	}
	if values := query["key"]; len(values) > 0 {
		forms++
		token = values[0]
	}
	if forms > 1 {
		return "", invalidRequest("invalid_request", "Provide the Gemini API key in one location.", nil)
	}
	if forms == 0 || token == "" {
		return "", authenticationError("invalid_api_key", "Provide a Gemini API key.")
	}
	return token, nil
}

func (s *Server) geminiLive(w http.ResponseWriter, r *http.Request) {
	x := &execution{request: s.begin(w, r), family: openai.FamilyGeminiLive, actor: "api_key", mode: "realtime"}
	status := http.StatusInternalServerError
	var out *outcome
	defer func() { s.finish(x, out, status) }()
	if !s.admit(r.Context()) {
		x.failure, status = overloaded, overloaded.Status
		writeSurfaceError(w, overloaded, "gemini")
		return
	}
	defer s.release(r.Context())
	if origin := r.Header.Get("Origin"); origin != "" && !slices.Contains(s.cfg.CORSAllowedOrigins, origin) {
		e := permissionError("origin_forbidden", "This origin is not allowed to open Live sessions.")
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, "gemini")
		return
	}
	if !realtimeUpgrade(r) {
		e := invalidRequest("websocket_upgrade_required", "Gemini Live requires a WebSocket upgrade.", nil)
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, "gemini")
		return
	}
	token, e := geminiClientKey(r)
	if e == nil {
		var authority access.Authority
		authority, e = s.authenticate(r, "inference")
		if e == nil {
			x.authority = authority
			x.keyID, x.affinity = authority.ID, []byte(authority.ID)
			x.budgetGroupID = authority.BudgetGroupID
			x.attribution, e = s.parseAttribution(r, authority)
		}
	}
	if e != nil {
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, "gemini")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), geminiLiveSession)
	defer cancel()
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		e := serverError(http.StatusBadRequest, "websocket_upgrade_failed", "The Live WebSocket upgrade failed.")
		x.failure, status = e, e.Status
		writeSurfaceError(w, e, "gemini")
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
		x.failure, status = invalidRequest("invalid_live_setup", "Gemini Live requires a setup message first.", nil), http.StatusBadRequest
		return
	}
	setup, err := geminilifecycle.ParseLiveSetup(initial, int(limit))
	if err != nil || !openai.RouteSlug.MatchString(setup.Model) {
		client.Close(websocket.StatusPolicyViolation, "invalid setup")
		x.failure, status = invalidRequest("invalid_live_setup", "Gemini Live setup is malformed.", nil), http.StatusBadRequest
		return
	}
	route, ok := x.request.release.Snapshot.Routes[setup.Model]
	if !ok || !x.authority.Allows("inference", route.Slug, route.ProjectID, s.now()) || !slices.Contains(route.Operations, "realtime") {
		client.Close(websocket.StatusPolicyViolation, "route unavailable")
		x.failure, status = modelNotFound(setup.Model), http.StatusNotFound
		return
	}
	x.route = &route
	if e := policySurfaceGate(&route); e != nil {
		client.Close(websocket.StatusPolicyViolation, "policy unavailable")
		x.failure, status = e, e.Status
		return
	}
	if x.preferences, e = routingPreferences(r); e != nil {
		client.Close(websocket.StatusPolicyViolation, "invalid routing")
		x.failure, status = e, e.Status
		return
	}
	x.estimate = resourceEstimate
	if e := s.reserveState(ctx, x, x.authority, geminiLiveSession); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.failure, status = e, e.Status
		return
	}
	var p *pin
	defer func() {
		settled := geminiLiveSettlement(x)
		if p != nil && p.hold != nil {
			p.hold.settle(r.Context(), x.dispatched, settled)
		}
		settleKey(r.Context(), x.lease, x.dispatched, settled, s.log)
	}()
	p, e = s.selectPinSurface(ctx, x, &route, "realtime", "gemini", "realtime", func(provider *runtime.Provider, model string) bool {
		return provider.ProfileID == "gemini-live" && provider.Connector().Supports("realtime", "gemini", "realtime") && provider.Supports(model, "realtime", "gemini", "realtime")
	})
	if e != nil {
		client.Close(websocket.StatusPolicyViolation, "provider unavailable")
		x.failure, status = e, e.Status
		return
	}
	endpoint, err := p.provider.Connector().GeminiLiveURL()
	if err != nil {
		client.Close(websocket.StatusPolicyViolation, "profile unavailable")
		x.failure, status = serverError(http.StatusBadGateway, "upstream_error", "The Live profile cannot address this model."), http.StatusBadGateway
		return
	}
	check := strings.Replace(strings.Replace(endpoint, "wss://", "https://", 1), "ws://", "http://", 1)
	if _, err := s.egress.ValidateEndpoint(check); err != nil {
		client.Close(websocket.StatusPolicyViolation, "provider unavailable")
		x.failure, status = serverError(http.StatusBadGateway, "upstream_error", "The Live provider address is not permitted."), http.StatusBadGateway
		return
	}
	bound, err := setup.BindModel(p.provider.Connector().Model(p.model))
	if err != nil || int64(len(bound)) > limit {
		client.Close(websocket.StatusPolicyViolation, "invalid setup")
		x.failure, status = invalidRequest("invalid_live_setup", "Gemini Live setup could not be bound.", nil), http.StatusBadRequest
		return
	}
	dialTimer := time.AfterFunc(p.attempt.Timeout, cancel)
	upstream, e := realtimeDial(ctx, s, x, p, endpoint)
	dialTimer.Stop()
	if e != nil {
		client.Close(websocket.StatusInternalError, "provider unavailable")
		x.failure, status = e, e.Status
		return
	}
	defer upstream.Close(websocket.StatusNormalClosure, "")
	upstream.SetReadLimit(limit)
	// A failed setup write may still have reached the provider. This request
	// owns one Attempt and never selects another provider after that boundary.
	x.dispatched = true
	if err := writeGeminiLiveFrame(ctx, upstream, websocket.MessageText, bound); err != nil {
		client.Close(websocket.StatusInternalError, "setup uncertain")
		x.failure, status = serverError(http.StatusBadGateway, "upstream_outcome_unknown", "The Live setup outcome is unknown."), http.StatusBadGateway
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class, fact.BillingUncertain = classAmbiguous, true
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = usage.UpstreamUnknown
			}
		}
		return
	}
	ackCtx, stopAck := context.WithTimeout(ctx, p.attempt.Timeout)
	ackType, ack, err := upstream.Read(ackCtx)
	stopAck()
	if err != nil {
		client.Close(websocket.StatusInternalError, "setup uncertain")
		x.failure, status = serverError(http.StatusBadGateway, "upstream_outcome_unknown", "The Live provider did not acknowledge setup."), http.StatusBadGateway
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class, fact.BillingUncertain = classAmbiguous, true
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = usage.UpstreamUnknown
			}
		}
		return
	}
	if ackType != websocket.MessageText || geminilifecycle.ValidateLiveServerFrame(ack, int(limit)) != nil {
		client.Close(websocket.StatusProtocolError, "invalid setup acknowledgement")
		x.failure, status = serverError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent a malformed setup acknowledgement."), http.StatusBadGateway
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class, fact.BillingUncertain = classProtocol, true
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = usage.UpstreamTerminal
			}
		}
		return
	}
	ackDocument, err := oif.ParseJSON(ack, oif.Limits{MaxBytes: int(limit)})
	if err != nil {
		client.Close(websocket.StatusProtocolError, "invalid setup acknowledgement")
		x.failure, status = serverError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent an invalid setup acknowledgement."), http.StatusBadGateway
		return
	}
	if _, ok := ackDocument.Root().Lookup("setupComplete"); !ok {
		client.Close(websocket.StatusProtocolError, "setup acknowledgement required")
		x.failure, status = serverError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent content before setup completion."), http.StatusBadGateway
		return
	}
	if err := writeGeminiLiveFrame(ctx, client, websocket.MessageText, ack); err != nil {
		x.failure, status = serverError(http.StatusBadGateway, "client_delivery_failed", "The Live setup acknowledgement could not be delivered."), http.StatusBadGateway
		return
	}
	x.delivered(s.now())
	if len(x.facts) > 0 && x.facts[len(x.facts)-1].Interaction != nil {
		x.facts[len(x.facts)-1].Interaction.UpstreamState = usage.UpstreamAccepted
		x.facts[len(x.facts)-1].Interaction.ClientState = usage.ClientPartial
	}
	usageRecord, usageComplete, relayErr := s.relayGeminiLive(ctx, x, p, client, upstream, token, int(limit))
	class := classProtocol
	cancelled := false
	if x.strict() && relayErr != nil {
		class = classAmbiguous
		switch {
		case errors.Is(relayErr, errGeminiLiveProviderError):
			class = classUpstreamServer
		case errors.Is(relayErr, errGeminiLiveClientClosed):
			class, cancelled = classCancelled, true
		case errors.Is(relayErr, errGeminiLiveClientProtocol), errors.Is(relayErr, errGeminiLiveProtocol):
			class = classProtocol
		case errors.Is(relayErr, errGeminiLiveAuthority):
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
				if errors.Is(relayErr, errGeminiLiveProviderError) {
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
		x.failure, status = serverError(http.StatusBadGateway, "live_incomplete", "The Live session ended without a complete transport contract."), http.StatusBadGateway
		if x.strict() {
			switch {
			case errors.Is(relayErr, errGeminiLiveProviderError):
				x.failure = serverError(http.StatusBadGateway, "live_provider_error", "The Live provider reported an error.")
			case errors.Is(relayErr, errGeminiLiveProtocol):
				x.failure = serverError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent an invalid frame.")
			case errors.Is(relayErr, errGeminiLiveClientProtocol):
				x.failure = invalidRequest("invalid_live_frame", "The Live client sent an invalid frame.", nil)
			case errors.Is(relayErr, errGeminiLiveAuthority):
				x.failure = permissionError("key_revoked", "The Live session no longer has route authority.")
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

func (s *Server) relayGeminiLive(ctx context.Context, x *execution, p *pin, client, upstream *websocket.Conn, token string, maxBytes int) (*openai.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var observed *openai.Usage
	var turn *openai.Usage
	var inFlight bool
	type relayEnd struct {
		err            error
		fromProvider   bool
		sourceClosed   bool
		deliveryFailed bool
	}
	forward := func(dst, src *websocket.Conn, fromProvider bool) relayEnd {
		end := func(err error) relayEnd { return relayEnd{err: err, fromProvider: fromProvider} }
		for {
			kind, payload, err := src.Read(ctx)
			if err != nil {
				return relayEnd{err: err, fromProvider: fromProvider, sourceClosed: true}
			}
			if kind != websocket.MessageText {
				if fromProvider {
					return end(errGeminiLiveProtocol)
				}
				return end(errGeminiLiveClientProtocol)
			}
			providerError := false
			if fromProvider {
				if err := geminilifecycle.ValidateLiveServerFrame(payload, maxBytes); err != nil {
					return end(fmt.Errorf("%w: %v", errGeminiLiveProtocol, err))
				}
				doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: maxBytes})
				if err != nil {
					return end(fmt.Errorf("%w: %v", errGeminiLiveProtocol, err))
				}
				if _, repeated := doc.Root().Lookup("setupComplete"); repeated {
					return end(fmt.Errorf("%w: repeated setup completion", errGeminiLiveProtocol))
				}
				usageValue := geminiLiveUsage(payload, maxBytes)
				turnComplete := geminiLiveTurnComplete(payload, maxBytes)
				_, serverContent := doc.Root().Lookup("serverContent")
				_, toolCall := doc.Root().Lookup("toolCall")
				_, providerError = doc.Root().Lookup("error")
				if x.strict() && (serverContent || toolCall) {
					mu.Lock()
					inFlight = !turnComplete
					mu.Unlock()
				}
				if usageValue != nil || turnComplete {
					mu.Lock()
					if usageValue != nil {
						turn = usageValue
					}
					if turnComplete && turn != nil {
						if observed == nil {
							observed = turn
						} else {
							observed.InputTokens = addBounded(observed.InputTokens, turn.InputTokens)
							observed.OutputTokens = addBounded(observed.OutputTokens, turn.OutputTokens)
							if turn.CachedInputTokens != nil {
								cached := *turn.CachedInputTokens
								if observed.CachedInputTokens != nil {
									cached = addBounded(cached, *observed.CachedInputTokens)
								}
								observed.CachedInputTokens = &cached
							}
						}
						turn = nil
					}
					mu.Unlock()
				}
			} else if err := geminilifecycle.ValidateLiveClientFrame(payload, maxBytes); err != nil {
				return end(fmt.Errorf("%w: %v", errGeminiLiveClientProtocol, err))
			} else if x.strict() {
				// A client message can start or continue model work. Retain only
				// whether a turn still needs its provider completion.
				mu.Lock()
				inFlight = true
				mu.Unlock()
			}
			if err := writeGeminiLiveFrame(ctx, dst, kind, payload); err != nil {
				return relayEnd{err: err, fromProvider: fromProvider, deliveryFailed: fromProvider}
			}
			if fromProvider {
				x.delivered(s.now())
				if x.strict() && providerError {
					return end(errGeminiLiveProviderError)
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
loop:
	for {
		select {
		case first = <-done:
			break loop
		case <-reauth.C:
			authority, err := s.Runtime.Authenticate(token)
			if err != nil || authority.ID != x.keyID || !authority.Allows("inference", x.route.Slug, x.route.ProjectID, s.now()) || p.slot.CredentialID != nil && s.Runtime.Revoked(*p.slot.CredentialID) || p.provider.Network != nil && p.provider.Network.CredentialID != "" && s.Runtime.Revoked(p.provider.Network.CredentialID) {
				first.err = errGeminiLiveAuthority
				client.Close(websocket.StatusPolicyViolation, "authority revoked")
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
	mu.Lock()
	pending := inFlight
	mu.Unlock()
	code := websocket.CloseStatus(first.err)
	if x.strict() && first.fromProvider && pending && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		first.err = fmt.Errorf("%w: %v", errGeminiLiveIncomplete, first.err)
		code = websocket.StatusInternalError
	}
	if x.strict() && errors.Is(first.err, errGeminiLiveProviderError) {
		code = websocket.StatusInternalError
	}
	if code < 0 {
		code = websocket.StatusInternalError
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		upstream.Close(code, "")
		client.Close(code, "")
	} else {
		// An abruptly disconnected peer cannot finish a close handshake. Tear
		// down both transports so a blocked opposite writer releases its slot.
		upstream.CloseNow()
		client.CloseNow()
	}
	second := <-done
	mu.Lock()
	defer mu.Unlock()
	if x.strict() && !errors.Is(first.err, context.DeadlineExceeded) {
		if first.deliveryFailed || !first.fromProvider && first.sourceClosed && (inFlight || second.deliveryFailed) {
			return observed, false, fmt.Errorf("%w: %v", errGeminiLiveClientClosed, first.err)
		}
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		return observed, turn == nil && observed != nil, nil
	}
	return observed, false, first.err
}

func geminiLiveTurnComplete(payload []byte, maxBytes int) bool {
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: maxBytes})
	if err != nil {
		return false
	}
	serverContent, ok := doc.Root().Lookup("serverContent")
	if !ok {
		return false
	}
	complete, ok := serverContent.Lookup("turnComplete")
	return ok && complete.Kind() == oif.Boolean && complete.Raw() == "true"
}

func geminiLiveUsage(payload []byte, maxBytes int) *openai.Usage {
	doc, err := oif.ParseJSON(payload, oif.Limits{MaxBytes: maxBytes})
	if err != nil {
		return nil
	}
	metadata, ok := doc.Root().Lookup("usageMetadata")
	if !ok || metadata.Kind() != oif.Object {
		return nil
	}
	var native struct {
		Input  *int64 `json:"promptTokenCount"`
		Output *int64 `json:"responseTokenCount"`
		Cached *int64 `json:"cachedContentTokenCount"`
	}
	if json.Unmarshal(metadata.Bytes(), &native) != nil || native.Input == nil || native.Output == nil || *native.Input < 0 || *native.Output < 0 || native.Cached != nil && (*native.Cached < 0 || *native.Cached > *native.Input) {
		return nil
	}
	usageValue := &openai.Usage{InputTokens: *native.Input, OutputTokens: *native.Output}
	if native.Cached != nil {
		usageValue.CachedInputTokens = native.Cached
	}
	return usageValue
}

type geminiLiveFrameWriter interface {
	Write(context.Context, websocket.MessageType, []byte) error
}

func writeGeminiLiveFrame(ctx context.Context, conn geminiLiveFrameWriter, kind websocket.MessageType, payload []byte) error {
	return writeGeminiLiveFrameWithin(ctx, conn, kind, payload, geminiLiveFrameWriteTimeout)
}

func writeGeminiLiveFrameWithin(ctx context.Context, conn geminiLiveFrameWriter, kind websocket.MessageType, payload []byte, timeout time.Duration) error {
	frameCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return conn.Write(frameCtx, kind, payload)
}

func geminiLiveSettlement(x *execution) *int64 {
	if !x.dispatched {
		zero := int64(0)
		return &zero
	}
	actual := totalTokens(x.usage())
	if len(x.facts) == 0 || !x.facts[len(x.facts)-1].BillingUncertain {
		return actual
	}
	conservative := x.estimate
	if actual != nil {
		conservative = max(conservative, *actual)
	}
	return &conservative
}
