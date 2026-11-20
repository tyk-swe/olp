package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/realtimeresponses"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	maxWebRTCSDPBytes     = 64 << 10
	maxWebRTCSessionBytes = 64 << 10
	webrtcCloseTimeout    = 10 * time.Second
	webrtcAnswerTimeout   = 30 * time.Second
)

type webrtcSession struct {
	id        string
	callID    string
	keyID     string
	token     string
	x         *kernel.Call
	p         *kernel.Pin
	conn      *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	started   time.Time
	rejected  bool
	admission *admission.Permit

	settleOnce sync.Once
	usageMu    sync.Mutex
	usage      wire.Usage
	haveUsage  bool
	responses  realtimeresponses.Tracker
}

func readWebRTCPart(part *multipart.Part, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("part too large")
	}
	return data, nil
}

func parseWebRTCCallRequest(r *http.Request) (*webrtcCallRequest, *fault.Error) {
	invalid := func(message string, param *string) (*webrtcCallRequest, *fault.Error) {
		return nil, fault.InvalidRequest("invalid_value", message, param)
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return invalid("The realtime call content type is malformed.", nil)
	}
	body := &io.LimitedReader{R: r.Body, N: bodylimit.Maximum + 1}
	out := &webrtcCallRequest{slug: r.URL.Query().Get("model")}
	switch mediaType {
	case "application/sdp":
		if len(r.URL.Query()["model"]) != 1 {
			return invalid("model must name exactly one published route slug.", new("model"))
		}
		data, err := io.ReadAll(io.LimitReader(body, maxWebRTCSDPBytes+1))
		if err != nil || len(data) == 0 || len(data) > maxWebRTCSDPBytes {
			return invalid("The SDP offer is missing or too large.", new("sdp"))
		}
		out.sdp = data
	case "multipart/form-data":
		reader := multipart.NewReader(body, params["boundary"])
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
					return invalid("The sdp part must appear once.", new("sdp"))
				}
				data, err := readWebRTCPart(part, maxWebRTCSDPBytes)
				if err != nil || len(data) == 0 {
					return invalid("The SDP offer is missing or too large.", new("sdp"))
				}
				out.sdp = data
			case "session":
				if out.session != nil {
					return invalid("The session part must appear once.", new("session"))
				}
				data, err := readWebRTCPart(part, maxWebRTCSessionBytes)
				if err != nil || len(data) == 0 {
					return invalid("The session document is missing or too large.", new("session"))
				}
				out.session = data
			default:
				return invalid("This realtime call field has no qualified binding.", new(part.FormName()))
			}
		}
		if out.sdp == nil {
			return invalid("The SDP offer is missing or too large.", new("sdp"))
		}
	default:
		return nil, fault.InvalidRequest("unsupported_media_type", "Realtime calls require multipart form data or an SDP body.", nil)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return invalid("The realtime call body could not be read.", nil)
	}
	if body.N == 0 {
		return nil, ingress.BodyTooLarge()
	}
	out.bodyBytes = bodylimit.Maximum + 1 - body.N
	return out, nil
}

func webrtcTranscriptionConfigured(members map[string]json.RawMessage) bool {
	configured := func(raw json.RawMessage) bool {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) != nil {
			return false
		}
		model, ok := nested["model"]
		return ok && string(model) != "null"
	}
	if raw, ok := members["input_audio_transcription"]; ok && configured(raw) {
		return true
	}
	var audio map[string]json.RawMessage
	if raw, ok := members["audio"]; !ok || json.Unmarshal(raw, &audio) != nil {
		return false
	}
	var input map[string]json.RawMessage
	if raw, ok := audio["input"]; !ok || json.Unmarshal(raw, &input) != nil {
		return false
	}
	raw, ok := input["transcription"]
	return ok && configured(raw)
}

func bindWebRTCSession(raw json.RawMessage, slug, providerModel string) (json.RawMessage, *fault.Error) {
	if len(raw) == 0 {
		bound, _ := json.Marshal(map[string]string{"type": "realtime", "model": providerModel})
		return bound, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil || members == nil {
		return nil, fault.InvalidRequest("invalid_value", "The session document must be one JSON object.", new("session"))
	}
	if rawType, present := members["type"]; present {
		var sessionType string
		if err := json.Unmarshal(rawType, &sessionType); err != nil || sessionType != "realtime" {
			return nil, fault.InvalidRequest("unsupported_session_type", "This surface only admits realtime sessions.", new("session"))
		}
	}
	members["type"] = json.RawMessage(`"realtime"`)
	for _, name := range []string{"client_secret", "client_secrets"} {
		if _, ok := members[name]; ok {
			return nil, fault.InvalidRequest("client_secrets_unavailable", "Raw provider client secrets are not available on this surface.", new("session"))
		}
	}
	if webrtcTranscriptionConfigured(members) {
		return nil, fault.InvalidRequest("unsupported_session_field", "Input transcription is not available on this surface.", new("session"))
	}
	if rawModel, ok := members["model"]; ok {
		var model string
		if err := json.Unmarshal(rawModel, &model); err != nil || model != slug {
			return nil, fault.InvalidRequest("invalid_value", "The session model must name the published route slug.", new("session"))
		}
	}
	encoded, _ := json.Marshal(providerModel)
	members["model"] = encoded
	bound, err := json.Marshal(members)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_value", "The session document must be one JSON object.", new("session"))
	}
	return bound, nil
}

func webrtcSessionSlug(raw json.RawMessage, querySlug string) (string, *fault.Error) {
	slug := querySlug
	if len(raw) > 0 {
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil || members == nil {
			return "", fault.InvalidRequest("invalid_value", "The session document must be one JSON object.", new("session"))
		}
		if rawModel, ok := members["model"]; ok {
			var model string
			if err := json.Unmarshal(rawModel, &model); err != nil {
				return "", fault.InvalidRequest("invalid_value", "The session model must name the published route slug.", new("session"))
			}
			if slug != "" && slug != model {
				return "", fault.InvalidRequest("invalid_value", "The session model must name the published route slug.", new("session"))
			}
			slug = model
		}
	}
	if !routeidentity.ValidSlug(slug) {
		return "", fault.InvalidRequest("invalid_value", "model must name a published route slug.", new("model"))
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

type callTurn struct {
	surface
	w         http.ResponseWriter
	r         *http.Request
	x         *kernel.Call
	authority keyauthority.Authority
	token     string
}

func (t callTurn) refuse(e *fault.Error) { t.kernel.Refuse(t.x, t.w, e) }

func (s surface) realtimeCallsCreate(w http.ResponseWriter, r *http.Request) {
	x := s.kernel.SessionCall(w, r, wire.Realtime, "webrtc")
	permit := admission.PermitFromContext(r.Context())
	if permit == nil {
		permit = s.kernel.Gate.Pool().AcquirePermit()
	}
	if permit == nil {
		s.kernel.Refuse(x, w, fault.Overloaded)
		return
	}
	defer permit.Release()
	authority, token, e := s.admitCallCaller(x, r, permit)
	if e != nil {
		s.kernel.Refuse(x, w, e)
		return
	}
	t := callTurn{surface: s, w: w, r: r, x: x, authority: authority, token: token}
	callReq, slug, controller, e := t.readCall()
	if e != nil {
		t.refuse(e)
		return
	}
	route, snapshot, e := t.routeForCall(slug, callReq)
	if e != nil {
		t.refuse(e)
		return
	}
	bg, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), webRTCBound)
	x.SessionEstimate()
	p, e := t.pinCall(bg, route)
	if e != nil {
		s.kernel.SettleCaps(r.Context(), x)
		cancel()
		t.refuse(e)
		return
	}
	releasePin := func() {
		s.kernel.SettleSessionHold(r.Context(), x, p.Hold, nil)
		s.kernel.SettleCaps(r.Context(), x)
	}
	boundSession, e := t.boundSession(snapshot, route, p, slug, callReq)
	if e != nil {
		releasePin()
		cancel()
		t.refuse(e)
		return
	}
	if e := s.kernel.ReserveState(bg, x, authority, webRTCBound); e != nil {
		releasePin()
		cancel()
		t.refuse(e)
		return
	}
	if e := s.kernel.ReservePinSessionDebit(bg, x, authority, p, nil); e != nil {
		s.kernel.ReleaseLease(r.Context(), x)
		releasePin()
		cancel()
		t.refuse(e)
		return
	}
	if e := s.kernel.ReserveSessionCost(bg, x, authority, p, webRTCBound); e != nil {
		s.kernel.AbandonSessionDebit(r.Context(), x)
		s.kernel.ReleaseLease(r.Context(), x)
		releasePin()
		cancel()
		t.refuse(e)
		return
	}
	sess := &webrtcSession{
		id: uuid.NewString(), keyID: authority.ID, token: token,
		x: x, p: p, ctx: bg, cancel: cancel, started: s.kernel.Now(),
		admission: permit.Transfer(),
	}
	s.calls.register(sess)
	t.launchCall(bg, cancel, sess, callReq, boundSession, controller)
}

func (s surface) admitCallCaller(x *kernel.Call, r *http.Request, permit *admission.Permit) (keyauthority.Authority, string, *fault.Error) {
	if !s.kernel.BrowserOriginAllowed(r) {
		return keyauthority.Authority{}, "", fault.PermissionError("origin_forbidden", "This origin is not allowed to open realtime sessions.")
	}
	token := ingress.BearerToken(r)
	if token == "" {
		return keyauthority.Authority{}, "", fault.AuthenticationError("missing_authorization", "Provide an Authorization Bearer [REDACTED] key.")
	}
	authority, e := s.kernel.Gate.AuthenticateRequest(r, token, "inference")
	if e != nil {
		return keyauthority.Authority{}, "", e
	}
	s.kernel.BindAuthority(x, authority)
	if e := x.RoutingControls(r, authority); e != nil {
		return keyauthority.Authority{}, "", e
	}
	if !permit.Await(r.Context(), admission.ClassOf(x.Priority()), time.Time{}) {
		return keyauthority.Authority{}, "", fault.Overloaded
	}
	if e := x.NoteAttributionValues(r.Header.Values(attribution.Header), authority); e != nil {
		return keyauthority.Authority{}, "", e
	}
	return authority, token, nil
}

func (t callTurn) readCall() (*webrtcCallRequest, string, *http.ResponseController, *fault.Error) {
	controller := http.NewResponseController(t.w)
	if err := controller.SetReadDeadline(t.kernel.Now().Add(kernel.RequestBodyTimeout)); err != nil {
		return nil, "", nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The request body could not be bounded.")
	}
	if t.kernel.Config.MaxBodyBytes > 0 {
		t.r.Body = http.MaxBytesReader(t.w, t.r.Body, t.kernel.Config.MaxBodyBytes)
	}
	callReq, e := parseWebRTCCallRequest(t.r)
	if e != nil {
		return nil, "", nil, e
	}
	_ = controller.SetReadDeadline(time.Time{})
	slug, e := webrtcSessionSlug(callReq.session, callReq.slug)
	if e != nil {
		return nil, "", nil, e
	}
	return callReq, slug, controller, nil
}

func (t callTurn) routeForCall(slug string, callReq *webrtcCallRequest) (*routing.Route, *routing.View, *fault.Error) {
	snapshot := t.x.Snapshot()
	route, ok := snapshot.FindRoute(slug)
	if !ok {
		return nil, snapshot, fault.ModelNotFound(slug)
	}
	t.x.Route = &route
	if !t.authority.Allows("inference", route.Slug, route.ProjectID, t.kernel.Now()) {
		return nil, snapshot, fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
	}
	if route.MaxBodyBytes != nil && callReq.bodyBytes > *route.MaxBodyBytes {
		return nil, snapshot, ingress.BodyTooLarge()
	}
	if !slices.Contains(route.Operations, "realtime") {
		return nil, snapshot, fault.InvalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow realtime sessions.", nil)
	}
	if e := t.x.GuardrailSurfaceGate(&route); e != nil {
		return nil, snapshot, e
	}
	if !t.x.Strict() {
		return nil, snapshot, kernel.PolicyUnavailable("webrtc_contract_unavailable", "Governed WebRTC requires a strict route with a compiled calls contract.")
	}
	if held, exposureErr := t.kernel.SessionExposureHeld(t.r.Context(), t.authority.ID); exposureErr != nil {
		return nil, snapshot, exposureErr
	} else if held {
		return nil, snapshot, fault.ServerError(http.StatusConflict, "webrtc_exposure_unresolved", "An earlier WebRTC session for this key is still billing-uncertain; new sessions are refused until that exposure resolves.")
	}
	return &route, snapshot, nil
}

func (t callTurn) pinCall(bg context.Context, route *routing.Route) (*kernel.Pin, *fault.Error) {
	return t.kernel.SelectPinSurface(bg, t.x, route, "realtime", "openai", "webrtc", func(provider *routing.Provider, model string) bool {
		return provider.Connector().Supports("realtime", "openai", "webrtc") && provider.Supports(model, "realtime", "openai", "webrtc")
	})
}

func (t callTurn) boundSession(snapshot *routing.View, route *routing.Route, p *kernel.Pin, slug string, callReq *webrtcCallRequest) (json.RawMessage, *fault.Error) {
	template, ok := snapshot.WebRTCTemplate(route.Slug, p.Target.ID)
	if !ok || !template.WebRTC() {
		return nil, kernel.PolicyUnavailable("webrtc_contract_unavailable", "The selected target has no compiled governed WebRTC contract.")
	}
	bound, e := bindWebRTCSession(callReq.session, slug, template.Model())
	if e != nil {
		return nil, e
	}
	if e := t.kernel.UnboundedPinLimits(t.x.AdmissionAuthority(t.authority), p, route); e != nil {
		return nil, e
	}
	return bound, nil
}

func (t callTurn) launchCall(bg context.Context, cancel context.CancelFunc, sess *webrtcSession, callReq *webrtcCallRequest, boundSession json.RawMessage, controller *http.ResponseController) {
	body, contentType, buildErr := buildWebRTCCallsBody(callReq.sdp, boundSession)
	if buildErr != nil {
		e := fault.ServerError(http.StatusInternalServerError, "internal_error", "The realtime call could not be encoded.")
		t.surface.settleWebRTC(sess, e)
		fault.WriteSurfaceError(t.w, e, "openai")
		return
	}
	created, e := t.kernel.CreateWebRTCCall(bg, t.x, sess.p, body, contentType, webrtcCloseTimeout, maxWebRTCSDPBytes)
	sess.rejected, sess.callID, sess.conn = created.Rejected, created.CallID, created.Conn
	if created.Exposure {
		t.kernel.HoldWebRTCExposure(sess.keyID, sess.started.Add(webRTCBound))
	}
	if e != nil {
		t.surface.settleWebRTC(sess, e)
		fault.WriteSurfaceError(t.w, e, "openai")
		return
	}
	t.deliverCall(bg, cancel, sess, created.Answer, created.Status, controller)
}

func (t callTurn) deliverCall(bg context.Context, cancel context.CancelFunc, sess *webrtcSession, answer []byte, status int, controller *http.ResponseController) {
	abandoned := fault.ServerError(http.StatusBadGateway, "realtime_incomplete", "The realtime session ended without a complete transport contract.")
	if t.r.Context().Err() != nil {
		if err := t.kernel.HangupWebRTCCall(bg, sess.x, sess.p, sess.callID, webrtcCloseTimeout); err != nil {
			t.kernel.HoldWebRTCExposure(sess.keyID, sess.started.Add(webRTCBound))
		}
		t.surface.settleWebRTC(sess, abandoned)
		cancel()
		return
	}
	go t.surface.runWebRTCControl(sess)
	t.w.Header().Set("Content-Type", "application/sdp")
	t.w.Header().Set("Location", "/v1/realtime/calls/"+sess.id)
	_ = controller.SetWriteDeadline(t.kernel.Now().Add(webrtcAnswerTimeout))
	t.w.WriteHeader(status)
	_, werr := t.w.Write(answer)
	if werr == nil {
		werr = controller.Flush()
	}
	if werr != nil || t.r.Context().Err() != nil {
		if herr := t.kernel.HangupWebRTCCall(bg, sess.x, sess.p, sess.callID, webrtcCloseTimeout); herr != nil {
			t.kernel.HoldWebRTCExposure(sess.keyID, sess.started.Add(webRTCBound))
		}
		t.surface.settleWebRTC(sess, abandoned)
		cancel()
	}
}

type webrtcCallRequest struct {
	sdp       []byte
	session   json.RawMessage
	slug      string
	bodyBytes int64
}
