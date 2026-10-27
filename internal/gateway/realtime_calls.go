package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

var (
	webrtcSessionBound    = time.Hour
	webrtcReauthInterval  = realtimeReauth
	webrtcHeartbeatPeriod = realtimePing
)

const (
	maxWebRTCSDPBytes     = 64 << 10
	maxWebRTCSessionBytes = 64 << 10
	webrtcCloseTimeout    = 10 * time.Second
)

type webrtcSession struct {
	id        string
	callID    string
	keyID     string
	token     string
	x         *call
	p         *pin
	conn      *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	started   time.Time
	closeReq  chan struct{}
	costBound usage.Cost

	settleOnce sync.Once
	closeOnce  sync.Once
	usageMu    sync.Mutex
	usage      wire.Usage
	haveUsage  bool
	responses  realtimeResponseState
}

func (s *Server) webrtcSessionCount() int {
	s.webrtcMu.Lock()
	defer s.webrtcMu.Unlock()
	return len(s.webrtcSessions)
}

func (s *Server) registerWebRTCSession(sess *webrtcSession) {
	s.webrtcMu.Lock()
	defer s.webrtcMu.Unlock()
	if s.webrtcSessions == nil {
		s.webrtcSessions = map[string]*webrtcSession{}
	}
	s.webrtcSessions[sess.id] = sess
}

func (s *Server) unregisterWebRTCSession(id string) {
	s.webrtcMu.Lock()
	defer s.webrtcMu.Unlock()
	delete(s.webrtcSessions, id)
}

func (s *Server) lookupWebRTCSession(id, keyID string) *webrtcSession {
	s.webrtcMu.Lock()
	defer s.webrtcMu.Unlock()
	sess := s.webrtcSessions[id]
	if sess == nil || sess.keyID != keyID {
		return nil
	}
	return sess
}

func (s *Server) holdWebRTCExposure(keyID string, until time.Time) {
	s.webrtcMu.Lock()
	defer s.webrtcMu.Unlock()
	if s.webrtcExposures == nil {
		s.webrtcExposures = map[string]time.Time{}
	}
	if current, ok := s.webrtcExposures[keyID]; !ok || until.After(current) {
		s.webrtcExposures[keyID] = until
	}
}

func (s *Server) webrtcExposureHeld(keyID string) bool {
	s.webrtcMu.Lock()
	defer s.webrtcMu.Unlock()
	until, ok := s.webrtcExposures[keyID]
	if !ok {
		return false
	}
	if s.now().After(until) {
		delete(s.webrtcExposures, keyID)
		return false
	}
	return true
}

type webrtcCallRequest struct {
	sdp     []byte
	session json.RawMessage
	slug    string
}

func readWebRTCPart(part *multipart.Part, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("part too large")
	}
	return data, nil
}

func parseWebRTCCallRequest(r *http.Request) (*webrtcCallRequest, *Error) {
	invalid := func(message string, param *string) (*webrtcCallRequest, *Error) {
		return nil, invalidRequest("invalid_value", message, param)
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return invalid("The realtime call content type is malformed.", nil)
	}
	out := &webrtcCallRequest{slug: r.URL.Query().Get("model")}
	switch mediaType {
	case "application/sdp":
		if len(r.URL.Query()["model"]) != 1 {
			return invalid("model must name exactly one published route slug.", strPtr("model"))
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxWebRTCSDPBytes+1))
		if err != nil || len(data) == 0 || len(data) > maxWebRTCSDPBytes {
			return invalid("The SDP offer is missing or too large.", strPtr("sdp"))
		}
		out.sdp = data
	case "multipart/form-data":
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return invalid("The realtime call form is malformed.", nil)
			}
			switch part.FormName() {
			case "sdp":
				if out.sdp != nil {
					return invalid("The sdp part must appear once.", strPtr("sdp"))
				}
				data, err := readWebRTCPart(part, maxWebRTCSDPBytes)
				if err != nil || len(data) == 0 {
					return invalid("The SDP offer is missing or too large.", strPtr("sdp"))
				}
				out.sdp = data
			case "session":
				if out.session != nil {
					return invalid("The session part must appear once.", strPtr("session"))
				}
				data, err := readWebRTCPart(part, maxWebRTCSessionBytes)
				if err != nil || len(data) == 0 {
					return invalid("The session document is missing or too large.", strPtr("session"))
				}
				out.session = data
			default:
				return invalid("This realtime call field has no qualified binding.", strPtr(part.FormName()))
			}
		}
		if out.sdp == nil {
			return invalid("The SDP offer is missing or too large.", strPtr("sdp"))
		}
	default:
		return nil, invalidRequest("unsupported_media_type", "Realtime calls require multipart form data or an SDP body.", nil)
	}
	return out, nil
}

func bindWebRTCSession(raw json.RawMessage, slug, providerModel string) (json.RawMessage, *Error) {
	if len(raw) == 0 {
		bound, _ := json.Marshal(map[string]string{"type": "realtime", "model": providerModel})
		return bound, nil
	}
	var members map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&members); err != nil || decoder.More() || members == nil {
		return nil, invalidRequest("invalid_value", "The session document must be one JSON object.", strPtr("session"))
	}
	for _, name := range []string{"client_secret", "client_secrets"} {
		if _, ok := members[name]; ok {
			return nil, invalidRequest("client_secrets_unavailable", "Raw provider client secrets are not available on this surface.", strPtr("session"))
		}
	}
	if rawModel, ok := members["model"]; ok {
		var model string
		if err := json.Unmarshal(rawModel, &model); err != nil || model != slug {
			return nil, invalidRequest("invalid_value", "The session model must name the published route slug.", strPtr("session"))
		}
	}
	encoded, _ := json.Marshal(providerModel)
	members["model"] = encoded
	bound, err := json.Marshal(members)
	if err != nil {
		return nil, invalidRequest("invalid_value", "The session document must be one JSON object.", strPtr("session"))
	}
	return bound, nil
}

func webrtcSessionSlug(raw json.RawMessage, querySlug string) (string, *Error) {
	slug := querySlug
	if len(raw) > 0 {
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil || members == nil {
			return "", invalidRequest("invalid_value", "The session document must be one JSON object.", strPtr("session"))
		}
		if rawModel, ok := members["model"]; ok {
			var model string
			if err := json.Unmarshal(rawModel, &model); err != nil {
				return "", invalidRequest("invalid_value", "The session model must name the published route slug.", strPtr("session"))
			}
			if slug != "" && slug != model {
				return "", invalidRequest("invalid_value", "The session model must name the published route slug.", strPtr("session"))
			}
			slug = model
		}
	}
	if !openai.RouteSlug.MatchString(slug) {
		return "", invalidRequest("invalid_value", "model must name a published route slug.", strPtr("model"))
	}
	return slug, nil
}

func buildWebRTCCallsBody(sdp, session []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	sdpPart, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="sdp"`},
		"Content-Type":        {"application/sdp"},
	})
	if err != nil {
		return nil, "", err
	}
	if _, err := sdpPart.Write(sdp); err != nil {
		return nil, "", err
	}
	sessionPart, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="session"`},
		"Content-Type":        {"application/json"},
	})
	if err != nil {
		return nil, "", err
	}
	if _, err := sessionPart.Write(session); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

func parseWebRTCCallID(location string) (string, bool) {
	parsed, err := url.Parse(location)
	if err != nil || parsed.Path == "" {
		return "", false
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) == 0 {
		return "", false
	}
	return segments[len(segments)-1], true
}

func (s *Server) realtimeCallsCreate(w http.ResponseWriter, r *http.Request) {
	x := &call{request: s.begin(w, r), family: wire.Realtime, actor: "api_key"}
	x.mode = "webrtc"
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
	token := bearerToken(r)
	if token == "" {
		fail(authenticationError("missing_authorization", "Provide an Authorization Bearer [REDACTED] key."))
		return
	}
	authority, e := s.authenticateRequest(r, token, "inference")
	if e != nil {
		fail(e)
		return
	}
	s.bindAuthority(x, authority)
	if x.attribution, e = parseAttributionValues(r.Header.Values(usage.AttributionHeader), authority); e != nil {
		fail(e)
		return
	}
	callReq, e := parseWebRTCCallRequest(r)
	if e != nil {
		fail(e)
		return
	}
	slug, e := webrtcSessionSlug(callReq.session, callReq.slug)
	if e != nil {
		fail(e)
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
	if e := x.guardrailSurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	if !x.strict() {
		fail(policyUnavailable("webrtc_contract_unavailable", "Governed WebRTC requires a strict route with a compiled calls contract."))
		return
	}
	if s.webrtcExposureHeld(authority.ID) {
		fail(serverError(http.StatusConflict, "webrtc_exposure_unresolved", "An earlier WebRTC session for this key is still billing-uncertain; new sessions are refused until that exposure resolves."))
		return
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), webrtcSessionBound)
	x.estimate = resourceEstimate
	p, e := s.selectPinSurface(bg, x, &route, "realtime", "openai", "webrtc", func(provider *routing.Provider, model string) bool {
		return provider.Connector().Supports("realtime", "openai", "webrtc") && provider.Supports(model, "realtime", "openai", "webrtc")
	})
	if e != nil {
		cancel()
		fail(e)
		return
	}
	template, ok := snapshot.WebRTCTemplate(route.Slug, p.target.ID)
	if !ok || !template.WebRTC() {
		s.settlePinHold(r.Context(), x, p.hold, nil)
		cancel()
		fail(policyUnavailable("webrtc_contract_unavailable", "The selected target has no compiled governed WebRTC contract."))
		return
	}
	boundSession, e := bindWebRTCSession(callReq.session, slug, template.Model())
	if e != nil {
		s.settlePinHold(r.Context(), x, p.hold, nil)
		cancel()
		fail(e)
		return
	}
	if e := s.reserveState(bg, x, authority, webrtcSessionBound); e != nil {
		s.settlePinHold(r.Context(), x, p.hold, nil)
		cancel()
		fail(e)
		return
	}
	if x.costBudgeted(authority) && x.attemptCostReservation(authority, p.attempt).amount == "" && !x.callerCostExempt() {
		settleKey(r.Context(), x.lease, false, nil, s.log)
		s.settlePinHold(r.Context(), x, p.hold, nil)
		cancel()
		fail(invalidRequest("unbounded_work_budget", "This operation cannot enforce token or cost budgets; use a bounded inference operation.", nil))
		return
	}
	if e := s.reserveSessionCost(bg, x, authority, p.attempt, webrtcSessionBound); e != nil {
		settleKey(r.Context(), x.lease, false, nil, s.log)
		s.settlePinHold(r.Context(), x, p.hold, nil)
		cancel()
		fail(e)
		return
	}
	sess := &webrtcSession{
		id: uuid.NewString(), keyID: authority.ID, token: token,
		x: x, p: p, ctx: bg, cancel: cancel, started: s.now(),
		closeReq: make(chan struct{}),
	}
	if x.costBudgeted(authority) && !x.callerCostExempt() {
		sess.costBound = x.attemptCostBound(p.attempt)
	}
	s.registerWebRTCSession(sess)
	answer, status, e := s.createWebRTCCall(bg, sess, callReq.sdp, boundSession)
	if e != nil {
		s.settleWebRTC(sess, e, false)
		writeSurfaceError(w, e, "openai")
		return
	}
	go s.runWebRTCControl(sess)
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/v1/realtime/calls/"+sess.id)
	w.WriteHeader(status)
	_, _ = w.Write(answer)
}

func (s *Server) createWebRTCCall(ctx context.Context, sess *webrtcSession, sdp, session []byte) ([]byte, int, *Error) {
	x, p := sess.x, sess.p
	cfg := p.provider.Connector()
	endpoint, err := cfg.RealtimeCallsURL(p.model)
	if err != nil {
		return nil, 0, serverError(http.StatusBadGateway, "upstream_error", "The provider profile cannot address this realtime model.")
	}
	if _, err := s.egress.ValidateEndpoint(endpoint); err != nil {
		return nil, 0, serverError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy.")
	}
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1, &p.provider)
	fact.Mode = "webrtc"
	if p.hold != nil {
		fact.Budgets = p.hold.budgets
	}
	state := attemptState{parent: ctx, classifier: upstream.Classifier{Declared: cfg.Classification()}}
	body, contentType, err := buildWebRTCCallsBody(sdp, session)
	if err != nil {
		return nil, 0, serverError(http.StatusInternalServerError, "internal_error", "The realtime call could not be encoded.")
	}
	reqCtx := httptrace.WithClientTrace(ctx, state.trace())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/sdp")
	req.Header.Set("User-Agent", "olp/gateway")
	if err := s.applySlotCredential(reqCtx, x, req, cfg, p.slot, body, &p.provider, p.model); err != nil {
		fact.Class = classCredential
		fact.Duration = s.now().Sub(fact.StartedAt)
		x.facts = append(x.facts, fact)
		return nil, 0, serverError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied.")
	}
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		fact.Class = classCredential
		fact.Duration = s.now().Sub(fact.StartedAt)
		x.facts = append(x.facts, fact)
		return nil, 0, serverError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable.")
	}
	resp, err := client.Do(req)
	x.dispatched = x.dispatched || state.dispatched.Load() || resp != nil
	if err != nil {
		fact.Class = classConnect
		if state.dispatched.Load() {
			fact.recordEvidence(true)
			s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
		}
		fact.Duration = s.now().Sub(fact.StartedAt)
		x.facts = append(x.facts, fact)
		return nil, 0, serverError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
	}
	defer resp.Body.Close()
	state.status = resp.StatusCode
	fact.Status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		nativeError := x.redacted(upstreamResponseError(resp))
		evidence := state.evidence()
		evidence.Error = nativeError
		outcome := state.classifier.Classify(evidence)
		fact.Class = string(outcome.Class)
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(false)
		x.facts = append(x.facts, fact)
		return nil, 0, upstreamError(&attemptFailure{status: resp.StatusCode, upstream: nativeError})
	}
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxWebRTCSDPBytes+1))
	if err != nil || len(answer) == 0 || len(answer) > maxWebRTCSDPBytes {
		fact.Class = classProtocol
		fact.recordEvidence(true)
		fact.Duration = s.now().Sub(fact.StartedAt)
		x.facts = append(x.facts, fact)
		s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
		return nil, 0, serverError(http.StatusBadGateway, "upstream_error", "The provider returned an unusable session answer.")
	}
	callID, ok := parseWebRTCCallID(resp.Header.Get("Location"))
	controlEndpoint, controlErr := cfg.RealtimeControlURL(p.model, callID)
	if !ok || controlErr != nil {
		fact.Class = classProtocol
		fact.recordEvidence(true)
		fact.Duration = s.now().Sub(fact.StartedAt)
		x.facts = append(x.facts, fact)
		s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
		return nil, 0, serverError(http.StatusBadGateway, "realtime_control_unavailable", "The provider session cannot be controlled by this gateway.")
	}
	sess.callID = callID
	fact.Class = "success"
	fact.Committed = true
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamAccepted
	}
	fact.Duration = s.now().Sub(fact.StartedAt)
	fact.recordEvidence(true)
	x.facts = append(x.facts, fact)
	conn, e := s.dialWebRTCControl(ctx, sess, controlEndpoint)
	if e != nil {
		if herr := s.hangupWebRTCCall(ctx, sess); herr != nil {
			s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
		}
		return nil, 0, e
	}
	sess.conn = conn
	return answer, resp.StatusCode, nil
}

func (s *Server) dialWebRTCControl(ctx context.Context, sess *webrtcSession, endpoint string) (*websocket.Conn, *Error) {
	x, p := sess.x, sess.p
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, serverError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	if err := s.applySlotCredential(ctx, x, probe, p.provider.Connector(), p.slot, nil, &p.provider, p.model); err != nil {
		return nil, serverError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied.")
	}
	headers := http.Header{}
	for name, values := range probe.Header {
		headers[name] = append([]string{}, values...)
	}
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		return nil, serverError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable.")
	}
	conn, _, err := websocket.Dial(ctx, probe.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
	if err != nil {
		return nil, serverError(http.StatusBadGateway, "realtime_control_unavailable", "The provider session cannot be controlled by this gateway.")
	}
	limit := s.cfg.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	conn.SetReadLimit(bodylimit.Lower(limit, x.route.MaxBodyBytes))
	return conn, nil
}

func (s *Server) hangupWebRTCCall(parent context.Context, sess *webrtcSession) error {
	if sess.callID == "" {
		return errors.New("no provider call identity")
	}
	cfg := sess.p.provider.Connector()
	endpoint, err := cfg.RealtimeHangupURL(sess.p.model, sess.callID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), webrtcCloseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if err := s.applySlotCredential(ctx, sess.x, req, cfg, sess.p.slot, nil, &sess.p.provider, sess.p.model); err != nil {
		return err
	}
	client, err := s.providerClient(ctx, sess.x.request.release, &sess.p.provider, sess.p.slot)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("provider hangup status %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) runWebRTCControl(sess *webrtcSession) {
	readErr := make(chan error, 1)
	go func() {
		for {
			typ, data, err := sess.conn.Read(sess.ctx)
			if err != nil {
				readErr <- err
				return
			}
			sess.usageMu.Lock()
			if u := sess.responses.providerFrame(typ, data, true); u != nil {
				sess.usage.InputTokens = addBounded(sess.usage.InputTokens, u.InputTokens)
				sess.usage.OutputTokens = addBounded(sess.usage.OutputTokens, u.OutputTokens)
				if u.CachedInputTokens != nil {
					cached := *u.CachedInputTokens
					if sess.usage.CachedInputTokens != nil {
						cached = addBounded(cached, *sess.usage.CachedInputTokens)
					}
					sess.usage.CachedInputTokens = &cached
				}
				sess.haveUsage = true
			}
			sess.usageMu.Unlock()
		}
	}()
	reauth := time.NewTicker(webrtcReauthInterval)
	defer reauth.Stop()
	heartbeat := time.NewTicker(webrtcHeartbeatPeriod)
	defer heartbeat.Stop()
	for {
		select {
		case err := <-readErr:
			s.finishWebRTCControl(sess, err)
			return
		case <-sess.closeReq:
			closed := s.hangupWebRTCCall(sess.ctx, sess) == nil
			sess.usageMu.Lock()
			complete := sess.haveUsage && !sess.responses.pending()
			sess.usageMu.Unlock()
			if closed && complete {
				s.settleWebRTC(sess, nil, true)
			} else {
				if !closed {
					s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
				}
				s.settleWebRTC(sess, serverError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract."), true)
			}
			sess.cancel()
			return
		case <-reauth.C:
			if e := s.webrtcBudgetExhausted(sess); e != nil {
				if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
					s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
				}
				s.settleWebRTC(sess, e, true)
				sess.cancel()
				return
			}
			if e := s.webrtcAuthority(sess); e != nil {
				if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
					s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
				}
				s.settleWebRTC(sess, e, true)
				sess.cancel()
				return
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(sess.ctx, webrtcReauthInterval)
			err := sess.conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				s.finishWebRTCControl(sess, err)
				return
			}
		case <-sess.ctx.Done():
			if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
				s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
			}
			s.settleWebRTC(sess, serverError(http.StatusGatewayTimeout, "realtime_session_expired", "The realtime session reached its maximum duration."), true)
			return
		}
	}
}

func (s *Server) webrtcBudgetExhausted(sess *webrtcSession) *Error {
	if sess.costBound.IsZero() || sess.p.attempt.Price == nil {
		return nil
	}
	sess.usageMu.Lock()
	if !sess.haveUsage {
		sess.usageMu.Unlock()
		return nil
	}
	observed := sess.usage
	sess.usageMu.Unlock()
	cost, ok := sess.p.attempt.Price.Cost(usage.ObservedAttemptUsage(&observed, true, "realtime"), sess.started, true, sess.p.attempt.Discount)
	if !ok || cost.Cmp(sess.costBound) <= 0 {
		return nil
	}
	return serverError(http.StatusTooManyRequests, "budget_exhausted", "The session cost budget is exhausted.")
}

func (s *Server) webrtcAuthority(sess *webrtcSession) *Error {
	x := sess.x
	authority, err := s.Runtime.Authenticate(sess.token)
	if err != nil || authority == nil || authority.ID != sess.keyID || !authority.Allows("inference", x.route.Slug, x.route.ProjectID, s.now()) || !authority.AllowsEndUser(x.endUserDigest) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.endUserDigest) || !authority.AllowsAttribution(x.attribution) || !authority.AllowsClientIP(x.request.clientIP) {
		return permissionError("key_revoked", "The realtime session no longer has route authority.")
	}
	if eligibility := s.pinEligibility(sess.p); eligibility != routing.Eligible {
		return permissionError("provider_credential_"+string(eligibility), "The realtime provider credential is no longer eligible.")
	}
	return nil
}

func (s *Server) finishWebRTCControl(sess *webrtcSession, readErr error) {
	code := websocket.CloseStatus(readErr)
	sess.usageMu.Lock()
	pending := sess.responses.pending()
	sess.usageMu.Unlock()
	if (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) && !pending && sess.ctx.Err() == nil {
		s.settleWebRTC(sess, nil, false)
		sess.cancel()
		return
	}
	if errors.Is(sess.ctx.Err(), context.DeadlineExceeded) {
		if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
			s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
		}
		s.settleWebRTC(sess, serverError(http.StatusGatewayTimeout, "realtime_session_expired", "The realtime session reached its maximum duration."), true)
		return
	}
	if err := s.hangupWebRTCCall(sess.ctx, sess); err != nil {
		s.holdWebRTCExposure(sess.keyID, sess.started.Add(webrtcSessionBound))
	}
	s.settleWebRTC(sess, serverError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract."), false)
	sess.cancel()
}

func (s *Server) settleWebRTC(sess *webrtcSession, failure *Error, _ bool) {
	sess.settleOnce.Do(func() {
		x := sess.x
		if sess.conn != nil {
			_ = sess.conn.CloseNow()
		}
		sess.usageMu.Lock()
		var observed *wire.Usage
		if sess.haveUsage {
			usageCopy := sess.usage
			observed = &usageCopy
		}
		pending := sess.responses.pending()
		sess.usageMu.Unlock()
		class := classProtocol
		cancelled := false
		if failure != nil {
			switch {
			case failure.Code == "key_revoked" || strings.HasPrefix(failure.Code, "provider_credential_"):
				class = classCredential
			case failure.Code == "realtime_session_expired":
				class = classTimeout
			}
		}
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			if fact.Interaction != nil {
				if observed != nil {
					fact.Interaction.ClientState = usage.ClientPartial
				}
				if failure == nil {
					fact.Interaction.UpstreamState = usage.UpstreamTerminal
					fact.Interaction.ClientState = usage.ClientTerminal
				} else {
					fact.Interaction.UpstreamState = usage.UpstreamUnknown
				}
			}
			if observed != nil {
				fact.Usage = observed
				fact.UsageObserved = true
				fact.UsageComplete = failure == nil && !pending
				fact.BillingUncertain = !fact.UsageComplete
			} else if x.dispatched {
				fact.UsageComplete = false
				fact.BillingUncertain = true
			}
			if failure != nil && fact.Class == "success" {
				fact.Class = class
			}
		}
		status := http.StatusOK
		var out *outcome
		if failure != nil {
			x.failure = failure
			status = failure.Status
			out = &outcome{err: failure, committed: x.dispatched, cancelled: cancelled}
		} else {
			out = &outcome{committed: true}
		}
		s.finish(x, out, status)
		settled := geminiLiveSettlement(x)
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(sess.ctx), webrtcCloseTimeout)
		s.settlePinHold(settleCtx, x, sess.p.hold, settled)
		s.recordCost(x)
		settleKey(settleCtx, x.lease, x.dispatched, settled, s.log)
		s.settleCaps(settleCtx, x)
		cancel()
		s.unregisterWebRTCSession(sess.id)
		sess.cancel()
	})
}

func (sess *webrtcSession) requestClose() {
	sess.closeOnce.Do(func() { close(sess.closeReq) })
}

func (s *Server) realtimeCallsHangup(w http.ResponseWriter, r *http.Request) {
	x := &call{request: s.begin(w, r), family: wire.Realtime, actor: "api_key"}
	x.mode = "webrtc"
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
	token := bearerToken(r)
	if token == "" {
		fail(authenticationError("missing_authorization", "Provide an Authorization Bearer [REDACTED] key."))
		return
	}
	authority, e := s.authenticateRequest(r, token, "inference")
	if e != nil {
		fail(e)
		return
	}
	s.bindAuthority(x, authority)
	sess := s.lookupWebRTCSession(r.PathValue("id"), authority.ID)
	if sess == nil {
		fail(serverError(http.StatusNotFound, "call_not_found", "No such realtime call exists for this key."))
		return
	}
	sess.requestClose()
	s.finish(x, &outcome{committed: false}, http.StatusOK)
	w.WriteHeader(http.StatusOK)
}
