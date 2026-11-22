package kernel

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/upstream"
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

type streamContextPeer struct {
	DuplexPeer
	cancel context.CancelFunc
}

func (p *streamContextPeer) Close(code websocket.StatusCode, reason string) error {
	defer p.cancel()
	return p.DuplexPeer.Close(code, reason)
}

func (p *streamContextPeer) CloseNow() error {
	defer p.cancel()
	return p.DuplexPeer.CloseNow()
}

type DuplexOpener func(ctx context.Context, auth *connectors.Auth, cfg connectors.Config, secret []byte, client *http.Client) (DuplexPeer, egress.Sensitive, error)

func (s *Server) DialCustomDuplex(ctx context.Context, x *Call, p *Pin, endpoint string, open DuplexOpener, classifyErr func(error) (string, *fault.Error)) (DuplexPeer, *fault.Error) {
	fact := s.newFact(x, p.attempt, p.Slot, len(x.facts)+1, &p.Provider)
	fact.Mode = "realtime"
	if p.Hold != nil {
		fact.Budgets = p.Hold.budgets
	}
	finish := func(class string, e *fault.Error) (DuplexPeer, *fault.Error) {
		fact.Class = class
		fact.Duration = s.Now().Sub(fact.StartedAt)
		fact.RecordEvidence(false)
		x.facts = append(x.facts, fact)
		return nil, e
	}
	if _, err := s.Egress.ValidateEndpoint(endpoint); err != nil {
		return finish(ClassConnect, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted."))
	}
	dialCtx, cancel := context.WithCancel(ctx)
	dialTimer := time.AfterFunc(p.attempt.Timeout, cancel)
	peer, err := s.openCustomDuplex(dialCtx, x, p, open)
	dialTimer.Stop()
	if err != nil {
		cancel()
		class, failure := classifyErr(err)
		return finish(class, failure)
	}
	fact.Class = "success"
	fact.Status = http.StatusOK
	fact.Committed = true
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = upstream.Accepted
	}
	fact.Duration = s.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(true)
	x.facts = append(x.facts, fact)
	x.dispatched = true
	if p.Hold != nil {
		x.spendCaps(p.Hold.budgets, p.attempt)
	}
	return &streamContextPeer{DuplexPeer: peer, cancel: cancel}, nil
}

func (s *Server) openCustomDuplex(ctx context.Context, x *Call, p *Pin, open DuplexOpener) (DuplexPeer, error) {
	secret, err := s.slotSecret(ctx, x, p.Slot, &p.Provider, p.Model)
	if err != nil {
		return nil, err
	}
	client, err := s.providerClient(ctx, x.request.release, &p.Provider, p.Slot)
	if err != nil {
		return nil, err
	}
	peer, applied, err := open(ctx, s.auth, p.Provider.Connector(), secret, client)
	x.sensitive.Include(applied)
	return peer, err
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

func (s *Server) DialPinnedWebSocket(ctx context.Context, x *Call, p *Pin, endpoint string, readLimit int64) (*websocket.Conn, *fault.Error) {
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
	if readLimit > 0 {
		conn.SetReadLimit(readLimit)
	}
	return conn, nil
}

func (s *Server) DoPinnedAuxPost(ctx context.Context, x *Call, p *Pin, endpoint string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if err := s.applySlotCredential(ctx, x, req, p.Provider.Connector(), p.Slot, nil, &p.Provider, p.Model); err != nil {
		return 0, err
	}
	client, err := s.providerClient(ctx, x.request.release, &p.Provider, p.Slot)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, ErrorBodyLimit))
	return resp.StatusCode, nil
}

type SessionAttempt struct {
	server *Server
	x      *Call
	fact   events.AttemptFact
}

func (a *SessionAttempt) Fail(class string) {
	a.fact.Class = class
	a.fact.RecordEvidence(true)
	a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
	a.x.facts = append(a.x.facts, a.fact)
}

func (a *SessionAttempt) Accept() {
	a.fact.Class = "success"
	a.fact.Committed = true
	if a.fact.Interaction != nil {
		a.fact.Interaction.UpstreamState = upstream.Accepted
	}
	a.fact.Duration = a.server.Now().Sub(a.fact.StartedAt)
	a.fact.RecordEvidence(true)
	a.x.facts = append(a.x.facts, a.fact)
}

type SessionCreateResult struct {
	Response *http.Response
	Attempt  *SessionAttempt
	Rejected bool
	Exposure bool
}

func (s *Server) BeginSessionCreate(ctx, createCtx context.Context, x *Call, p *Pin, mode, endpoint, contentType, accept string, body []byte) (SessionCreateResult, *fault.Error) {
	fact := s.newFact(x, p.attempt, p.Slot, len(x.facts)+1, &p.Provider)
	fact.Mode = mode
	if p.Hold != nil {
		fact.Budgets = p.Hold.budgets
	}
	state := &attemptState{parent: createCtx, classifier: upstream.Classifier{Declared: p.Provider.Connector().Classification()}}
	reqCtx := httptrace.WithClientTrace(createCtx, state.trace())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return SessionCreateResult{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "olp/gateway")
	if err := s.applySlotCredential(reqCtx, x, req, p.Provider.Connector(), p.Slot, body, &p.Provider, p.Model); err != nil {
		s.recordSessionFailure(x, fact, ClassCredential)
		return SessionCreateResult{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider credential could not be applied.")
	}
	client, err := s.providerClient(ctx, x.request.release, &p.Provider, p.Slot)
	if err != nil {
		s.recordSessionFailure(x, fact, ClassCredential)
		return SessionCreateResult{}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider network credential is unavailable.")
	}
	if e := s.MarkSessionDispatched(reqCtx, x); e != nil {
		s.recordSessionFailure(x, fact, ClassConnect)
		return SessionCreateResult{}, e
	}
	resp, err := client.Do(req)
	x.dispatched = x.dispatched || state.dispatched.Load() || resp != nil
	if x.dispatched && p.Hold != nil {
		x.spendCaps(p.Hold.budgets, p.attempt)
	}
	if err != nil {
		return s.classifySessionSendError(x, fact, state)
	}
	state.status, fact.Status = resp.StatusCode, resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		nativeError := x.redacted(upstreamResponseError(resp))
		evidence := state.evidence()
		evidence.Error = nativeError
		outcome := state.classifier.Classify(evidence)
		s.recordSessionFailure(x, fact, string(outcome.Class))
		return SessionCreateResult{Rejected: outcome.Acceptance == upstream.Terminal}, UpstreamError(&AttemptFailure{Status: resp.StatusCode, upstream: nativeError})
	}
	return SessionCreateResult{Response: resp, Attempt: &SessionAttempt{server: s, x: x, fact: fact}}, nil
}

func (s *Server) recordSessionFailure(x *Call, fact events.AttemptFact, class string) {
	fact.Class = class
	fact.Duration = s.Now().Sub(fact.StartedAt)
	fact.RecordEvidence(false)
	x.facts = append(x.facts, fact)
}

func (s *Server) classifySessionSendError(x *Call, fact events.AttemptFact, state *attemptState) (SessionCreateResult, *fault.Error) {
	dispatched := state.dispatched.Load()
	rejected := state.classifier.Classify(state.evidence()).Acceptance == upstream.NotSent
	fact.Class = ClassConnect
	fact.RecordEvidence(dispatched)
	fact.Duration = s.Now().Sub(fact.StartedAt)
	x.facts = append(x.facts, fact)
	return SessionCreateResult{Rejected: rejected, Exposure: dispatched}, fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider could not complete the request.")
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
