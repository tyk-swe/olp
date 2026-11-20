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

type webrtcCall struct {
	server         *Server
	ctx            context.Context
	createCtx      context.Context
	x              *Call
	p              *Pin
	endpoint       string
	body           []byte
	contentType    string
	closeTimeout   time.Duration
	maxAnswerBytes int64
}

type webrtcCallSent struct {
	resp     *http.Response
	fact     events.AttemptFact
	state    *attemptState
	rejected bool
	exposure bool
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
	fact := s.newFact(x, p.attempt, p.Slot, len(x.facts)+1, &p.Provider)
	fact.Mode = "webrtc"
	if p.Hold != nil {
		fact.Budgets = p.Hold.budgets
	}
	createCtx := ctx
	cancelCreate := context.CancelFunc(func() {})
	if p.attempt.Timeout > 0 {
		createCtx, cancelCreate = context.WithTimeout(ctx, p.Timeout())
	}
	defer cancelCreate()
	state := attemptState{parent: createCtx, classifier: upstream.Classifier{Declared: cfg.Classification()}}
	call := webrtcCall{server: s, ctx: ctx, createCtx: createCtx, x: x, p: p, endpoint: endpoint, body: body, contentType: contentType, closeTimeout: closeTimeout, maxAnswerBytes: maxAnswerBytes}
	sent, e := call.send(fact, &state)
	if e != nil {
		return WebRTCCallResult{Rejected: sent.rejected, Exposure: sent.exposure}, e
	}
	defer sent.resp.Body.Close()
	return call.accept(sent)
}

func (c webrtcCall) send(fact events.AttemptFact, state *attemptState) (webrtcCallSent, *fault.Error) {
	rejected, exposure := false, false
	reqCtx := httptrace.WithClientTrace(c.createCtx, state.trace())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint, bytes.NewReader(c.body))
	if err != nil {
		return webrtcCallSent{fact: fact, state: state, rejected: rejected, exposure: exposure}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	req.Header.Set("Content-Type", c.contentType)
	req.Header.Set("Accept", "application/sdp")
	req.Header.Set("User-Agent", "olp/gateway")
	if err := c.server.applySlotCredential(reqCtx, c.x, req, c.p.Provider.Connector(), c.p.Slot, c.body, &c.p.Provider, c.p.Model); err != nil {
		fact.Class = ClassCredential
		fact.Duration = c.server.Now().Sub(fact.StartedAt)
		fact.RecordEvidence(false)
		c.x.facts = append(c.x.facts, fact)
		return webrtcCallSent{fact: fact, state: state, rejected: rejected, exposure: exposure}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied.")
	}
	client, err := c.server.providerClient(c.ctx, c.x.request.release, &c.p.Provider, c.p.Slot)
	if err != nil {
		fact.Class = ClassCredential
		fact.Duration = c.server.Now().Sub(fact.StartedAt)
		fact.RecordEvidence(false)
		c.x.facts = append(c.x.facts, fact)
		return webrtcCallSent{fact: fact, state: state, rejected: rejected, exposure: exposure}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable.")
	}
	if e := c.server.MarkSessionDispatched(reqCtx, c.x); e != nil {
		fact.Class = ClassConnect
		fact.Duration = c.server.Now().Sub(fact.StartedAt)
		fact.RecordEvidence(false)
		c.x.facts = append(c.x.facts, fact)
		return webrtcCallSent{fact: fact, state: state, rejected: rejected, exposure: exposure}, e
	}
	resp, err := client.Do(req)
	c.x.dispatched = c.x.dispatched || state.dispatched.Load() || resp != nil
	if c.x.dispatched && c.p.Hold != nil {
		c.x.spendCaps(c.p.Hold.budgets, c.p.attempt)
	}
	if err != nil {
		fact.Class = ClassConnect
		dispatched := state.dispatched.Load()
		rejected = state.classifier.Classify(state.evidence()).Acceptance == upstream.NotSent
		fact.RecordEvidence(dispatched)
		if dispatched {
			exposure = true
		}
		fact.Duration = c.server.Now().Sub(fact.StartedAt)
		c.x.facts = append(c.x.facts, fact)
		return webrtcCallSent{fact: fact, state: state, rejected: rejected, exposure: exposure}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
	}
	return webrtcCallSent{resp: resp, fact: fact, state: state, rejected: rejected, exposure: exposure}, nil
}

func (c webrtcCall) accept(sent webrtcCallSent) (WebRTCCallResult, *fault.Error) {
	resp, fact, state := sent.resp, sent.fact, sent.state
	result := WebRTCCallResult{Rejected: sent.rejected, Exposure: sent.exposure}
	state.status = resp.StatusCode
	fact.Status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		nativeError := c.x.redacted(upstreamResponseError(resp))
		evidence := state.evidence()
		evidence.Error = nativeError
		outcome := state.classifier.Classify(evidence)
		result.Rejected = outcome.Acceptance == upstream.Terminal
		fact.Class = string(outcome.Class)
		fact.Duration = c.server.Now().Sub(fact.StartedAt)
		fact.RecordEvidence(false)
		c.x.facts = append(c.x.facts, fact)
		return result, UpstreamError(&AttemptFailure{Status: resp.StatusCode, upstream: nativeError})
	}
	callID, ok := parseWebRTCCallID(resp.Header.Get("Location"))
	answer, err := io.ReadAll(io.LimitReader(resp.Body, c.maxAnswerBytes+1))
	mediaType, _, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || len(answer) == 0 || int64(len(answer)) > c.maxAnswerBytes || mediaErr != nil || mediaType != "application/sdp" {
		fact.Class = ClassProtocol
		fact.RecordEvidence(true)
		fact.Duration = c.server.Now().Sub(fact.StartedAt)
		c.x.facts = append(c.x.facts, fact)
		if !ok || c.server.HangupWebRTCCall(c.ctx, c.x, c.p, callID, c.closeTimeout) != nil {
			result.Exposure = true
		}
		return result, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned an unusable session answer.")
	}
	controlEndpoint, controlErr := c.p.Provider.Connector().RealtimeControlURL(c.p.Model, callID)
	if !ok || controlErr != nil {
		fact.Class = ClassProtocol
		fact.RecordEvidence(true)
		fact.Duration = c.server.Now().Sub(fact.StartedAt)
		c.x.facts = append(c.x.facts, fact)
		result.Exposure = true
		return result, fault.ServerError(http.StatusBadGateway, "realtime_control_unavailable", "The provider session cannot be controlled by this gateway.")
	}
	fact.Class = "success"
	fact.Committed = true
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = upstream.Accepted
	}
	fact.Duration = c.server.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(true)
	c.x.facts = append(c.x.facts, fact)
	conn, e := c.server.DialWebRTCControl(c.createCtx, c.x, c.p, controlEndpoint)
	if e != nil {
		if herr := c.server.HangupWebRTCCall(c.ctx, c.x, c.p, callID, c.closeTimeout); herr != nil {
			result.Exposure = true
		}
		return result, e
	}
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
