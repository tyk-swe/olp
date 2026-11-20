package kernel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/platform/panics"
	acceptance "github.com/tyk-swe/olp/internal/upstream"

	"github.com/coder/websocket"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/routing"
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
	mux.HandleFunc("OPTIONS /gemini/ws/", s.Preflight)
}

// geminiClientKey validates all SDK-supported key locations before the shared
// authority authenticates one value. Duplicate keys or competing locations
// must not silently select the first query/header value.
func GeminiClientKey(r *http.Request, allowedQuery ...string) (string, *fault.Error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", fault.InvalidRequest("invalid_request", "The Gemini query is malformed.", nil)
	}
	for name, values := range query {
		if !slices.Contains(append([]string{"key"}, allowedQuery...), name) || len(values) != 1 || values[0] == "" {
			return "", fault.InvalidRequest("invalid_request", "The Gemini query has an unsupported or repeated setting.", nil)
		}
	}
	forms := 0
	token := ""
	if values := r.Header.Values("Authorization"); len(values) > 0 {
		forms++
		if len(values) != 1 {
			return "", fault.InvalidRequest("invalid_request", "Provide one Gemini API key.", nil)
		}
		token = ingress.BearerToken(r)
	}
	if values := r.Header.Values("X-Goog-Api-Key"); len(values) > 0 {
		forms++
		if len(values) != 1 || values[0] == "" {
			return "", fault.InvalidRequest("invalid_request", "Provide one Gemini API key.", nil)
		}
		token = strings.TrimSpace(values[0])
	}
	if values := query["key"]; len(values) > 0 {
		forms++
		token = strings.TrimSpace(values[0])
	}
	if forms > 1 {
		return "", fault.InvalidRequest("invalid_request", "Provide the Gemini API key in one location.", nil)
	}
	if forms == 0 || token == "" {
		return "", fault.AuthenticationError("invalid_api_key", "Provide a Gemini API key.")
	}
	return token, nil
}

func (s *Server) geminiLive(w http.ResponseWriter, r *http.Request) {
	x := &Call{request: s.begin(w, r), family: wire.GeminiLive, actor: "api_key", mode: "realtime"}
	status := http.StatusInternalServerError
	var out *Outcome
	defer func() { s.Finish(x, out, status) }()
	if !s.Gate.Admit(r.Context()) {
		x.Failure, status = fault.Overloaded, fault.Overloaded.Status
		fault.WriteSurfaceError(w, fault.Overloaded, "gemini")
		return
	}
	defer s.Gate.Release(r.Context())
	if !s.browserOriginAllowed(r) {
		e := fault.PermissionError("origin_forbidden", "This origin is not allowed to open Live sessions.")
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, "gemini")
		return
	}
	if !realtimeUpgrade(r) {
		e := fault.InvalidRequest("websocket_upgrade_required", "Gemini Live requires a WebSocket upgrade.", nil)
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, "gemini")
		return
	}
	token, e := GeminiClientKey(r)
	if e == nil {
		var authority keyauthority.Authority
		authority, e = s.Gate.AuthenticateRequest(r, token, "inference")
		if e == nil {
			s.bindAuthority(x, authority)
			x.attribution, e = ingress.ParseAttribution(r, authority)
		}
	}
	if e != nil {
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, "gemini")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), geminiLiveSession)
	defer cancel()
	// Like a realtime upgrade, this one carries no allowance or metadata headers.
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		e := fault.ServerError(http.StatusBadRequest, "websocket_upgrade_failed", "The Live WebSocket upgrade failed.")
		x.Failure, status = e, e.Status
		fault.WriteSurfaceError(w, e, "gemini")
		return
	}
	defer client.Close(websocket.StatusNormalClosure, "")
	limit := s.Config.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	client.SetReadLimit(limit)
	setupCtx, stopSetup := context.WithTimeout(ctx, RequestBodyTimeout)
	messageType, initial, err := client.Read(setupCtx)
	stopSetup()
	if err != nil || messageType != websocket.MessageText {
		client.Close(websocket.StatusPolicyViolation, "setup required")
		x.Failure, status = fault.InvalidRequest("invalid_live_setup", "Gemini Live requires a setup message first.", nil), http.StatusBadRequest
		return
	}
	setup, err := geminilifecycle.ParseLiveSetup(initial, int(limit))
	if err != nil || !routeidentity.ValidSlug(setup.Model) {
		client.Close(websocket.StatusPolicyViolation, "invalid setup")
		x.Failure, status = fault.InvalidRequest("invalid_live_setup", "Gemini Live setup is malformed.", nil), http.StatusBadRequest
		return
	}
	route, ok := x.request.release.Snapshot().FindRoute(setup.Model)
	if !ok || !x.authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) || !slices.Contains(route.Operations, "realtime") {
		client.Close(websocket.StatusPolicyViolation, "route unavailable")
		x.Failure, status = fault.ModelNotFound(setup.Model), http.StatusNotFound
		return
	}
	x.Route = &route
	if route.MaxBodyBytes != nil && int64(len(initial)) > *route.MaxBodyBytes {
		client.Close(websocket.StatusMessageTooBig, "request body limit exceeded")
		x.Failure, status = ingress.BodyTooLarge(), http.StatusRequestEntityTooLarge
		return
	}
	client.SetReadLimit(bodylimit.Lower(limit, route.MaxBodyBytes))
	if e := x.GuardrailSurfaceGate(&route); e != nil {
		client.Close(websocket.StatusPolicyViolation, "policy unavailable")
		x.Failure, status = e, e.Status
		return
	}
	if x.preferences, x.priority, e = routingControls(r, x.authority.Policy); e != nil {
		client.Close(websocket.StatusPolicyViolation, "invalid routing")
		x.Failure, status = e, e.Status
		return
	}
	if e := UnboundedWorkLimits(x.AdmissionAuthority(x.authority), nil, nil, &route); e != nil {
		client.Close(websocket.StatusPolicyViolation, "unbounded work cannot enforce budgets")
		x.Failure, status = e, e.Status
		return
	}
	x.estimate = resourceEstimate
	if e := s.reserveState(ctx, x, x.authority, geminiLiveSession); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.Failure, status = e, e.Status
		return
	}
	var p *Pin
	defer func() { s.settleSession(r.Context(), x, p) }()
	p, e = s.SelectPinSurface(ctx, x, &route, "realtime", "gemini", "realtime", func(provider *routing.Provider, model string) bool {
		return provider.ProfileID == "gemini-live" && provider.Connector().Supports("realtime", "gemini", "realtime") && provider.Supports(model, "realtime", "gemini", "realtime")
	})
	if e != nil {
		client.Close(websocket.StatusPolicyViolation, "provider unavailable")
		x.Failure, status = e, e.Status
		return
	}
	if e := s.UnboundedPinLimits(x.AdmissionAuthority(x.authority), p, &route); e != nil {
		client.Close(websocket.StatusPolicyViolation, "unbounded work cannot enforce budgets")
		x.Failure, status = e, e.Status
		return
	}
	if e := s.reserveSessionCost(ctx, x, x.authority, p.attempt, geminiLiveSession); e != nil {
		client.Close(websocket.StatusPolicyViolation, "admission refused")
		x.Failure, status = e, e.Status
		return
	}
	endpoint, err := p.Provider.Connector().GeminiLiveURL()
	if err != nil {
		client.Close(websocket.StatusPolicyViolation, "profile unavailable")
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "upstream_error", "The Live profile cannot address this model."), http.StatusBadGateway
		return
	}
	check := strings.Replace(strings.Replace(endpoint, "wss://", "https://", 1), "ws://", "http://", 1)
	if _, err := s.Egress.ValidateEndpoint(check); err != nil {
		client.Close(websocket.StatusPolicyViolation, "provider unavailable")
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "upstream_error", "The Live provider address is not permitted."), http.StatusBadGateway
		return
	}
	bound, err := setup.BindModel(p.Provider.Connector().Model(p.Model))
	if err != nil || int64(len(bound)) > limit {
		client.Close(websocket.StatusPolicyViolation, "invalid setup")
		x.Failure, status = fault.InvalidRequest("invalid_live_setup", "Gemini Live setup could not be bound.", nil), http.StatusBadRequest
		return
	}
	dialTimer := time.AfterFunc(p.attempt.Timeout, cancel)
	upstream, e := realtimeDial(ctx, s, x, p, endpoint)
	dialTimer.Stop()
	if e != nil {
		client.Close(websocket.StatusInternalError, "provider unavailable")
		x.Failure, status = e, e.Status
		return
	}
	defer upstream.Close(websocket.StatusNormalClosure, "")
	upstream.SetReadLimit(limit)
	// A failed setup write may still have reached the provider. This request
	// owns one Attempt and never selects another provider after that boundary.
	x.dispatched = true
	if err := writeGeminiLiveFrame(ctx, upstream, websocket.MessageText, bound); err != nil {
		client.Close(websocket.StatusInternalError, "setup uncertain")
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "upstream_outcome_unknown", "The Live setup outcome is unknown."), http.StatusBadGateway
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class, fact.BillingUncertain = classAmbiguous, true
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = acceptance.Unknown
			}
		}
		return
	}
	ackCtx, stopAck := context.WithTimeout(ctx, p.attempt.Timeout)
	ackType, ack, err := upstream.Read(ackCtx)
	stopAck()
	if err != nil {
		client.Close(websocket.StatusInternalError, "setup uncertain")
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "upstream_outcome_unknown", "The Live provider did not acknowledge setup."), http.StatusBadGateway
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class, fact.BillingUncertain = classAmbiguous, true
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = acceptance.Unknown
			}
		}
		return
	}
	if ackType != websocket.MessageText || geminilifecycle.ValidateLiveServerFrame(ack, int(limit)) != nil {
		client.Close(websocket.StatusProtocolError, "invalid setup acknowledgement")
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent a malformed setup acknowledgement."), http.StatusBadGateway
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.Class, fact.BillingUncertain = ClassProtocol, true
			if fact.Interaction != nil {
				fact.Interaction.UpstreamState = acceptance.Terminal
			}
		}
		return
	}
	ackDocument, err := oif.ParseJSON(ack, oif.Limits{MaxBytes: int(limit)})
	if err != nil {
		client.Close(websocket.StatusProtocolError, "invalid setup acknowledgement")
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent an invalid setup acknowledgement."), http.StatusBadGateway
		return
	}
	if _, ok := ackDocument.Root().Lookup("setupComplete"); !ok {
		client.Close(websocket.StatusProtocolError, "setup acknowledgement required")
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent content before setup completion."), http.StatusBadGateway
		return
	}
	if err := writeGeminiLiveFrame(ctx, client, websocket.MessageText, ack); err != nil {
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The Live setup acknowledgement could not be delivered."), http.StatusBadGateway
		return
	}
	x.Delivered(s.Now())
	if len(x.facts) > 0 && x.facts[len(x.facts)-1].Interaction != nil {
		x.facts[len(x.facts)-1].Interaction.UpstreamState = acceptance.Accepted
		x.facts[len(x.facts)-1].Interaction.ClientState = usage.ClientPartial
	}
	usageRecord, usageComplete, relayErr := s.relayGeminiLive(ctx, x, p, client, upstream, token, int(limit))
	class := ClassProtocol
	cancelled := false
	if x.Strict() && relayErr != nil {
		class = classAmbiguous
		switch {
		case errors.Is(relayErr, errGeminiLiveProviderError):
			class = ClassUpstreamServer
		case errors.Is(relayErr, errGeminiLiveClientClosed):
			class, cancelled = ClassCancelled, true
		case errors.Is(relayErr, errGeminiLiveClientProtocol), errors.Is(relayErr, errGeminiLiveProtocol):
			class = ClassProtocol
		case errors.Is(relayErr, errGeminiLiveAuthority):
			class = classCredential
		case errors.Is(relayErr, context.Canceled):
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
				if errors.Is(relayErr, errGeminiLiveProviderError) {
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
		x.Failure, status = fault.ServerError(http.StatusBadGateway, "live_incomplete", "The Live session ended without a complete transport contract."), http.StatusBadGateway
		if x.Strict() {
			switch {
			case errors.Is(relayErr, errGeminiLiveProviderError):
				x.Failure = fault.ServerError(http.StatusBadGateway, "live_provider_error", "The Live provider reported an error.")
			case errors.Is(relayErr, errGeminiLiveProtocol):
				x.Failure = fault.ServerError(http.StatusBadGateway, "provider_protocol_error", "The Live provider sent an invalid frame.")
			case errors.Is(relayErr, errGeminiLiveClientProtocol):
				x.Failure = fault.InvalidRequest("invalid_live_frame", "The Live client sent an invalid frame.", nil)
			case errors.Is(relayErr, errGeminiLiveAuthority):
				x.Failure = fault.PermissionError("key_revoked", "The Live session no longer has route authority.")
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

func (s *Server) relayGeminiLive(ctx context.Context, x *Call, p *Pin, client, upstream duplexPeer, token string, maxBytes int) (*wire.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var observed *wire.Usage
	var turn *wire.Usage
	var turnGrounding int64
	var inFlight bool
	type relayEnd struct {
		err            error
		fromProvider   bool
		sourceClosed   bool
		deliveryFailed bool
	}
	forward := func(dst, src duplexPeer, fromProvider bool) relayEnd {
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
				frame, err := geminilifecycle.ObserveLiveServerFrame(payload, maxBytes)
				if err != nil {
					return end(fmt.Errorf("%w: %v", errGeminiLiveProtocol, err))
				}
				if frame.Kind == "setupComplete" {
					return end(fmt.Errorf("%w: repeated setup completion", errGeminiLiveProtocol))
				}
				doc := frame.Source
				var usageValue *wire.Usage
				if u := frame.Usage; u != nil {
					usageValue = geminiExtendedUsage(*u.Input, *u.Output, u.Cached, u.Thoughts, u.ToolUse, u.Total)
					geminiLiveModality(usageValue, u)
				}
				turnComplete := frame.TurnComplete
				serverContent, toolCall := frame.Kind == "serverContent", frame.Kind == "toolCall"
				if serverContent {
					// Grounded turns bill per query; the grounded prompt marker is a
					// flag the completion turns on once.
					if content, ok := doc.Lookup("serverContent"); ok {
						if grounding, ok := content.Lookup("groundingMetadata"); ok {
							if queries, ok := grounding.Lookup("webSearchQueries"); ok && queries.Kind() == oif.Array {
								turnGrounding += int64(len(queries.Elements()))
							}
						}
					}
				}
				providerError = frame.Kind == "error"
				if providerError {
					// An in-band provider error can echo an applied credential;
					// scrub it before the frame reaches the client.
					scrubbed, err := fault.RedactNativeFailureDocument(doc, x.sensitive)
					if err != nil {
						return end(fmt.Errorf("%w: provider error could not be redacted", errGeminiLiveProtocol))
					}
					payload = scrubbed.Bytes()
				}
				if x.Strict() && (serverContent || toolCall) {
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
						if turnGrounding > 0 {
							turn.ToolCalls = map[string]int64{
								"google_search_query":  turnGrounding,
								"google_search_prompt": 1,
							}
							turnGrounding = 0
						}
						if observed == nil {
							observed = turn
						} else {
							observed.InputTokens = quota.AddBounded(observed.InputTokens, turn.InputTokens)
							observed.OutputTokens = quota.AddBounded(observed.OutputTokens, turn.OutputTokens)
							observed.TotalTokens = quota.AddBounded(observed.TotalTokens, turn.TotalTokens)
							if turn.ReasoningTokens != nil {
								reasoning := *turn.ReasoningTokens
								if observed.ReasoningTokens != nil {
									reasoning = quota.AddBounded(reasoning, *observed.ReasoningTokens)
								}
								observed.ReasoningTokens = &reasoning
							}
							if turn.CachedInputTokens != nil {
								cached := *turn.CachedInputTokens
								if observed.CachedInputTokens != nil {
									cached = quota.AddBounded(cached, *observed.CachedInputTokens)
								}
								observed.CachedInputTokens = &cached
							}
							mergeUsageShare(&observed.AudioInputTokens, turn.AudioInputTokens)
							mergeUsageShare(&observed.CachedAudioInputTokens, turn.CachedAudioInputTokens)
							mergeUsageShare(&observed.AudioOutputTokens, turn.AudioOutputTokens)
							mergeUsageShare(&observed.ImageInputTokens, turn.ImageInputTokens)
							mergeUsageShare(&observed.CachedImageInputTokens, turn.CachedImageInputTokens)
							mergeUsageShare(&observed.ImageOutputTokens, turn.ImageOutputTokens)
							if len(turn.ToolCalls) > 0 {
								if observed.ToolCalls == nil {
									observed.ToolCalls = turn.ToolCalls
								} else {
									for tool, invocations := range turn.ToolCalls {
										observed.ToolCalls[tool] = quota.AddBounded(observed.ToolCalls[tool], invocations)
									}
									if observed.ToolCalls["google_search_prompt"] > 1 {
										observed.ToolCalls["google_search_prompt"] = 1
									}
								}
							}
						}
						turn = nil
					}
					mu.Unlock()
				}
			} else if err := geminilifecycle.ValidateLiveClientFrame(payload, maxBytes); err != nil {
				return end(fmt.Errorf("%w: %v", errGeminiLiveClientProtocol, err))
			} else if x.Strict() {
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
				x.Delivered(s.Now())
				if x.Strict() && providerError {
					return end(errGeminiLiveProviderError)
				}
			}
		}
	}
	done := make(chan relayEnd, 2)
	remaining := 2
	relay := func(dst, src duplexPeer, fromProvider bool) {
		defer panics.Recover(s.Log, "gemini live relay", func(err error) { done <- relayEnd{err: err} })
		done <- forward(dst, src, fromProvider)
	}
	go relay(upstream, client, false)
	go relay(client, upstream, true)
	reauth := time.NewTicker(ingress.ReauthInterval)
	defer reauth.Stop()
	heartbeat := time.NewTicker(realtimePing)
	defer heartbeat.Stop()
	var first relayEnd
loop:
	for {
		select {
		case first = <-done:
			remaining--
			break loop
		case <-reauth.C:
			authority, err := s.reauthenticateSession(ctx, x, token)
			if err != nil || authority.ID != x.keyID || !authority.Allows("inference", x.Route.Slug, x.Route.ProjectID, s.Now()) || !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) || s.pinEligibility(p) != routing.Eligible {
				first.err = errGeminiLiveAuthority
				client.Close(websocket.StatusPolicyViolation, "authority revoked")
				break loop
			}
			current := *authority
			current.EndUserDigest = x.endUserDigest
			if e := s.UnboundedPinLimits(x.AdmissionAuthority(current), p, x.Route); e != nil {
				first.err = errGeminiLiveAuthority
				client.Close(websocket.StatusPolicyViolation, "consumption budget changed")
				break loop
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, ingress.ReauthInterval)
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
	if x.Strict() && first.fromProvider && pending && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		first.err = fmt.Errorf("%w: %v", errGeminiLiveIncomplete, first.err)
		code = websocket.StatusInternalError
	}
	if x.Strict() && errors.Is(first.err, errGeminiLiveProviderError) {
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
	deliveryFailed := first.deliveryFailed
	for ; remaining > 0; remaining-- {
		end := <-done
		deliveryFailed = deliveryFailed || end.deliveryFailed
	}
	mu.Lock()
	defer mu.Unlock()
	if x.Strict() && !errors.Is(first.err, context.DeadlineExceeded) {
		if first.deliveryFailed || !first.fromProvider && first.sourceClosed && (inFlight || deliveryFailed) {
			return observed, false, fmt.Errorf("%w: %v", errGeminiLiveClientClosed, first.err)
		}
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		return observed, turn == nil && observed != nil, nil
	}
	return observed, false, first.err
}

// mergeUsageShare accumulates one optional usage share across turns.
func mergeUsageShare(slot **int64, value *int64) {
	if value == nil {
		return
	}
	total := *value
	if *slot != nil {
		total = quota.AddBounded(total, **slot)
	}
	*slot = &total
}

// geminiUsageDetails maps a Live frame's modality detail arrays onto the usage
// shares they price through: video frames bill as image input.
func geminiUsageDetails(detail []geminilifecycle.LiveUsageDetail) (audio, image int64) {
	for _, entry := range detail {
		switch entry.Modality {
		case "AUDIO":
			audio = quota.AddBounded(audio, entry.TokenCount)
		case "IMAGE", "VIDEO":
			image = quota.AddBounded(image, entry.TokenCount)
		}
	}
	return audio, image
}

// geminiLiveModality applies a Live frame's detail arrays to its usage.
func geminiLiveModality(u *wire.Usage, native *geminilifecycle.LiveUsage) {
	if u == nil {
		return
	}
	if audio, image := geminiUsageDetails(native.PromptDetails); audio > 0 || image > 0 {
		if audio > 0 {
			u.AudioInputTokens = &audio
		}
		if image > 0 {
			u.ImageInputTokens = &image
		}
	}
	if audio, image := geminiUsageDetails(native.CacheDetails); audio > 0 || image > 0 {
		if audio > 0 {
			u.CachedAudioInputTokens = &audio
		}
		if image > 0 {
			u.CachedImageInputTokens = &image
		}
	}
	if audio, image := geminiUsageDetails(native.ResponseDetails); audio > 0 || image > 0 {
		if audio > 0 {
			u.AudioOutputTokens = &audio
		}
		if image > 0 {
			u.ImageOutputTokens = &image
		}
	}
}

// geminiExtendedUsage folds the thinking and tool-use counts Gemini reports
// apart from its prompt and response counts into output and input usage, as
// the generateContent codec does, so every billed token reaches settlement.
// It returns nil when any reported count is negative.
func geminiExtendedUsage(input, output int64, cached, thoughts, toolUse, total *int64) *wire.Usage {
	for _, v := range []*int64{thoughts, toolUse, total} {
		if v != nil && *v < 0 {
			return nil
		}
	}
	usageValue := &wire.Usage{CachedInputTokens: cached}
	if toolUse != nil {
		input = quota.AddBounded(input, *toolUse)
	}
	if thoughts != nil {
		output = quota.AddBounded(output, *thoughts)
		reasoning := *thoughts
		usageValue.ReasoningTokens = &reasoning
	}
	usageValue.InputTokens, usageValue.OutputTokens = input, output
	usageValue.TotalTokens = quota.AddBounded(input, output)
	if total != nil {
		usageValue.TotalTokens = max(usageValue.TotalTokens, *total)
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
