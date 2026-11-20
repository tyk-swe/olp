package kernel

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/domain/bodylimit"
	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/upstream"
)

const (
	realtimePing        = 30 * time.Second
	webRTCBound         = time.Hour
	webrtcCloseTimeout  = 10 * time.Second
	nativeDuplexSession = time.Hour
)

// DuplexPeer is the transport a session owns. Its two forwarding tasks must
// finish after cancellation and peer closure before their observations are read.
// Protocol validation and terminal obligations stay with each session owner.
type DuplexPeer interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Ping(context.Context) error
	Close(websocket.StatusCode, string) error
	CloseNow() error
}

func (s *Server) RealtimeDial(ctx context.Context, x *Call, p *Pin, endpoint string) (*websocket.Conn, *fault.Error) {
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
		return nil, finish(ClassCredential, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied."))
	}
	headers := http.Header{}
	for name, values := range probe.Header {
		headers[name] = append([]string{}, values...)
	}
	client, err := s.providerClient(ctx, x.request.release, &p.Provider, p.Slot)
	if err != nil {
		return nil, finish(ClassCredential, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable."))
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, ErrorBodyLimit))
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

func (s *Server) DialWebRTCControl(ctx context.Context, x *Call, p *Pin, endpoint string) (*websocket.Conn, *fault.Error) {
	probe, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	if err := s.applySlotCredential(ctx, x, probe, p.Provider.Connector(), p.Slot, nil, &p.Provider, p.Model); err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied.")
	}
	headers := http.Header{}
	for name, values := range probe.Header {
		headers[name] = append([]string{}, values...)
	}
	client, err := s.providerClient(ctx, x.request.release, &p.Provider, p.Slot)
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable.")
	}
	conn, _, err := websocket.Dial(ctx, probe.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "realtime_control_unavailable", "The provider session cannot be controlled by this gateway.")
	}
	limit := s.Config.MaxEventBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	conn.SetReadLimit(bodylimit.Lower(limit, x.Route.MaxBodyBytes))
	return conn, nil
}

func (s *Server) HangupWebRTCCall(parent context.Context, x *Call, p *Pin, callID string, closeTimeout time.Duration) error {
	if callID == "" {
		return errors.New("no provider call identity")
	}
	cfg := p.Provider.Connector()
	endpoint, err := cfg.RealtimeHangupURL(p.Model, callID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), closeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if err := s.applySlotCredential(ctx, x, req, cfg, p.Slot, nil, &p.Provider, p.Model); err != nil {
		return err
	}
	client, err := s.providerClient(ctx, x.request.release, &p.Provider, p.Slot)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, ErrorBodyLimit))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("provider hangup status %d", resp.StatusCode)
	}
	return nil
}

func RealtimeUpgrade(r *http.Request) bool {
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

type WebRTCCallResult struct {
	Answer   []byte
	Status   int
	CallID   string
	Conn     *websocket.Conn
	Rejected bool
	Exposure bool
}

type webrtcCallAttempt struct {
	server         *Server
	ctx            context.Context
	createCtx      context.Context
	x              *Call
	p              *Pin
	fact           events.AttemptFact
	state          attemptState
	rejected       bool
	exposure       bool
	closeTimeout   time.Duration
	maxAnswerBytes int64
}

func (a *webrtcCallAttempt) result() WebRTCCallResult {
	return WebRTCCallResult{Rejected: a.rejected, Exposure: a.exposure}
}

func (s *Server) CreateWebRTCCall(ctx context.Context, x *Call, p *Pin, body []byte, contentType string, closeTimeout time.Duration, maxAnswerBytes int64) (WebRTCCallResult, *fault.Error) {
	cfg := p.Provider.Connector()
	endpoint, err := cfg.RealtimeCallsURL(p.Model)
	if err != nil {
		return WebRTCCallResult{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider profile cannot address this realtime model.")
	}
	if _, err := s.Egress.ValidateEndpoint(endpoint); err != nil {
		return WebRTCCallResult{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy.")
	}
	a := &webrtcCallAttempt{server: s, ctx: ctx, x: x, p: p, closeTimeout: closeTimeout, maxAnswerBytes: maxAnswerBytes}
	a.fact = s.newFact(x, p.attempt, p.Slot, len(x.facts)+1, &p.Provider)
	a.fact.Mode = "webrtc"
	if p.Hold != nil {
		a.fact.Budgets = p.Hold.budgets
	}
	a.createCtx = ctx
	cancelCreate := context.CancelFunc(func() {})
	if p.attempt.Timeout > 0 {
		a.createCtx, cancelCreate = context.WithTimeout(ctx, p.Timeout())
	}
	defer cancelCreate()
	a.state = attemptState{parent: a.createCtx, classifier: upstream.Classifier{Declared: cfg.Classification()}}
	resp, result, e := a.send(endpoint, body, contentType)
	if e != nil {
		return result, e
	}
	defer resp.Body.Close()
	return a.accept(resp)
}

func (a *webrtcCallAttempt) send(endpoint string, body []byte, contentType string) (*http.Response, WebRTCCallResult, *fault.Error) {
	reqCtx := httptrace.WithClientTrace(a.createCtx, a.state.trace())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, a.result(), fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/sdp")
	req.Header.Set("User-Agent", "olp/gateway")
	if err := a.server.applySlotCredential(reqCtx, a.x, req, a.p.Provider.Connector(), a.p.Slot, body, &a.p.Provider, a.p.Model); err != nil {
		a.fact.Class = ClassCredential
		a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
		a.fact.RecordEvidence(false)
		a.x.facts = append(a.x.facts, a.fact)
		return nil, a.result(), fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied.")
	}
	client, err := a.server.providerClient(a.ctx, a.x.request.release, &a.p.Provider, a.p.Slot)
	if err != nil {
		a.fact.Class = ClassCredential
		a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
		a.fact.RecordEvidence(false)
		a.x.facts = append(a.x.facts, a.fact)
		return nil, a.result(), fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable.")
	}
	if e := a.server.MarkSessionDispatched(reqCtx, a.x); e != nil {
		a.fact.Class = ClassConnect
		a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
		a.fact.RecordEvidence(false)
		a.x.facts = append(a.x.facts, a.fact)
		return nil, a.result(), e
	}
	resp, err := client.Do(req)
	a.x.dispatched = a.x.dispatched || a.state.dispatched.Load() || resp != nil
	if a.x.dispatched && a.p.Hold != nil {
		a.x.spendCaps(a.p.Hold.budgets, a.p.attempt)
	}
	if err != nil {
		a.fact.Class = ClassConnect
		dispatched := a.state.dispatched.Load()
		a.rejected = a.state.classifier.Classify(a.state.evidence()).Acceptance == upstream.NotSent
		a.fact.RecordEvidence(dispatched)
		if dispatched {
			a.exposure = true
		}
		a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
		a.x.facts = append(a.x.facts, a.fact)
		return nil, a.result(), fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
	}
	return resp, a.result(), nil
}

func (a *webrtcCallAttempt) accept(resp *http.Response) (WebRTCCallResult, *fault.Error) {
	a.state.status = resp.StatusCode
	a.fact.Status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		nativeError := a.x.redacted(upstreamResponseError(resp))
		evidence := a.state.evidence()
		evidence.Error = nativeError
		outcome := a.state.classifier.Classify(evidence)
		a.rejected = outcome.Acceptance == upstream.Terminal
		a.fact.Class = string(outcome.Class)
		a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
		a.fact.RecordEvidence(false)
		a.x.facts = append(a.x.facts, a.fact)
		return a.result(), UpstreamError(&AttemptFailure{Status: resp.StatusCode, upstream: nativeError})
	}
	callID, ok := parseWebRTCCallID(resp.Header.Get("Location"))
	answer, err := io.ReadAll(io.LimitReader(resp.Body, a.maxAnswerBytes+1))
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || len(answer) == 0 || int64(len(answer)) > a.maxAnswerBytes || mediaErr != nil || mediaType != "application/sdp" {
		a.fact.Class = ClassProtocol
		a.fact.RecordEvidence(true)
		a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
		a.x.facts = append(a.x.facts, a.fact)
		if !ok || a.server.HangupWebRTCCall(a.ctx, a.x, a.p, callID, a.closeTimeout) != nil {
			a.exposure = true
		}
		return a.result(), fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned an unusable session answer.")
	}
	controlEndpoint, controlErr := a.p.Provider.Connector().RealtimeControlURL(a.p.Model, callID)
	if !ok || controlErr != nil {
		a.fact.Class = ClassProtocol
		a.fact.RecordEvidence(true)
		a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
		a.x.facts = append(a.x.facts, a.fact)
		a.exposure = true
		return a.result(), fault.ServerError(http.StatusBadGateway, "realtime_control_unavailable", "The provider session cannot be controlled by this gateway.")
	}
	a.fact.Class = "success"
	a.fact.Committed = true
	if a.fact.Interaction != nil {
		a.fact.Interaction.UpstreamState = upstream.Accepted
	}
	a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
	a.fact.RecordEvidence(true)
	a.x.facts = append(a.x.facts, a.fact)
	conn, e := a.server.DialWebRTCControl(a.createCtx, a.x, a.p, controlEndpoint)
	if e != nil {
		if herr := a.server.HangupWebRTCCall(a.ctx, a.x, a.p, callID, a.closeTimeout); herr != nil {
			a.exposure = true
		}
		return a.result(), e
	}
	result := a.result()
	result.Answer, result.Status, result.CallID, result.Conn = answer, resp.StatusCode, callID, conn
	return result, nil
}

const duplexFrameWriteTimeout = 5 * time.Second

type duplexFrameWriter interface {
	Write(context.Context, websocket.MessageType, []byte) error
}

func WriteDuplexFrame(ctx context.Context, conn duplexFrameWriter, kind websocket.MessageType, payload []byte) error {
	return writeDuplexFrameWithin(ctx, conn, kind, payload, duplexFrameWriteTimeout)
}

func writeDuplexFrameWithin(ctx context.Context, conn duplexFrameWriter, kind websocket.MessageType, payload []byte, timeout time.Duration) error {
	frameCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return conn.Write(frameCtx, kind, payload)
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

// GeminiUsageDetails maps a Live frame's modality detail arrays onto the usage shares they price through.
func GeminiUsageDetails(detail []geminilifecycle.LiveUsageDetail) (audio, image int64) {
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

func GeminiExtendedUsage(input, output int64, cached, thoughts, toolUse, total *int64) *wire.Usage {
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
