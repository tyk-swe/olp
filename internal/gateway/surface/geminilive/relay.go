package geminilive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/routing"
)

type relayEnd struct {
	err            error
	fromProvider   bool
	sourceClosed   bool
	deliveryFailed bool
}

type liveRelay struct {
	surface
	ctx      context.Context
	x        *kernel.Call
	p        *kernel.Pin
	client   *websocket.Conn
	upstream *websocket.Conn
	token    string
	maxBytes int
	mu       sync.Mutex
	observed *wire.Usage
	turn     *wire.Usage
	ground   int64
	inFlight bool
}

func (s surface) relayLive(ctx context.Context, x *kernel.Call, p *kernel.Pin, client, upstream *websocket.Conn, token string, maxBytes int) (*wire.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st := &liveRelay{surface: s, ctx: ctx, x: x, p: p, client: client, upstream: upstream, token: token, maxBytes: maxBytes}
	done := make(chan relayEnd, 2)
	relay := func(dst, src *websocket.Conn, fromProvider bool) {
		defer panics.Recover(s.kernel.Log, "gemini live relay", func(err error) { done <- relayEnd{err: err} })
		done <- st.forward(dst, src, fromProvider)
	}
	go relay(upstream, client, false)
	go relay(client, upstream, true)
	first, remaining := st.watch(done)
	cancel()
	return st.converge(first, remaining, done)
}

func (st *liveRelay) forward(dst, src *websocket.Conn, fromProvider bool) relayEnd {
	for {
		kind, payload, err := src.Read(st.ctx)
		if err != nil {
			return relayEnd{err: err, fromProvider: fromProvider, sourceClosed: true}
		}
		if kind != websocket.MessageText {
			if fromProvider {
				return relayEnd{err: errLiveProtocol, fromProvider: fromProvider}
			}
			return relayEnd{err: errLiveClientProtocol, fromProvider: fromProvider}
		}
		providerError := false
		if fromProvider {
			out, perr, ferr := st.providerPayload(payload)
			if ferr != nil {
				return relayEnd{err: ferr, fromProvider: fromProvider}
			}
			payload, providerError = out, perr
		} else if err := geminilifecycle.ValidateLiveClientFrame(payload, st.maxBytes); err != nil {
			return relayEnd{err: fmt.Errorf("%w: %v", errLiveClientProtocol, err), fromProvider: fromProvider}
		} else if st.x.Strict() {
			// A client message can start or continue model work. Retain only
			// whether a turn still needs its provider completion.
			st.mu.Lock()
			st.inFlight = true
			st.mu.Unlock()
		}
		if err := kernel.WriteDuplexFrame(st.ctx, dst, kind, payload); err != nil {
			return relayEnd{err: err, fromProvider: fromProvider, deliveryFailed: fromProvider}
		}
		if fromProvider {
			st.x.Delivered(st.kernel.Now())
			if st.x.Strict() && providerError {
				return relayEnd{err: errLiveProviderError, fromProvider: fromProvider}
			}
		}
	}
}

func (st *liveRelay) providerPayload(payload []byte) ([]byte, bool, error) {
	frame, err := geminilifecycle.ObserveLiveServerFrame(payload, st.maxBytes)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", errLiveProtocol, err)
	}
	if frame.Kind == "setupComplete" {
		return nil, false, fmt.Errorf("%w: repeated setup completion", errLiveProtocol)
	}
	doc := frame.Source
	var usageValue *wire.Usage
	if u := frame.Usage; u != nil {
		usageValue = kernel.GeminiExtendedUsage(*u.Input, *u.Output, u.Cached, u.Thoughts, u.ToolUse, u.Total)
		geminiLiveModality(usageValue, u)
	}
	if frame.Kind == "serverContent" {
		// Grounded turns bill per query; the grounded prompt marker is a
		// flag the completion turns on once.
		if content, ok := doc.Lookup("serverContent"); ok {
			if grounding, ok := content.Lookup("groundingMetadata"); ok {
				if queries, ok := grounding.Lookup("webSearchQueries"); ok && queries.Kind() == oif.Array {
					st.ground += int64(len(queries.Elements()))
				}
			}
		}
	}
	out := payload
	if frame.Kind == "error" {
		// An in-band provider error can echo an applied credential;
		// scrub it before the frame reaches the client.
		scrubbed, err := fault.RedactNativeFailureDocument(doc, st.x.Sensitive())
		if err != nil {
			return nil, false, fmt.Errorf("%w: provider error could not be redacted", errLiveProtocol)
		}
		out = scrubbed.Bytes()
	}
	if st.x.Strict() && (frame.Kind == "serverContent" || frame.Kind == "toolCall") {
		st.mu.Lock()
		st.inFlight = !frame.TurnComplete
		st.mu.Unlock()
	}
	if usageValue != nil || frame.TurnComplete {
		st.commitTurn(usageValue, frame.TurnComplete)
	}
	return out, frame.Kind == "error", nil
}

func (st *liveRelay) commitTurn(usageValue *wire.Usage, turnComplete bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if usageValue != nil {
		st.turn = usageValue
	}
	if !(turnComplete && st.turn != nil) {
		return
	}
	turn := st.turn
	if st.ground > 0 {
		turn.ToolCalls = map[string]int64{"google_search_query": st.ground, "google_search_prompt": 1}
		st.ground = 0
	}
	if st.observed == nil {
		st.observed = turn
		st.turn = nil
		return
	}
	st.observed.InputTokens = quota.AddBounded(st.observed.InputTokens, turn.InputTokens)
	st.observed.OutputTokens = quota.AddBounded(st.observed.OutputTokens, turn.OutputTokens)
	st.observed.TotalTokens = quota.AddBounded(st.observed.TotalTokens, turn.TotalTokens)
	if turn.ReasoningTokens != nil {
		reasoning := *turn.ReasoningTokens
		if st.observed.ReasoningTokens != nil {
			reasoning = quota.AddBounded(reasoning, *st.observed.ReasoningTokens)
		}
		st.observed.ReasoningTokens = &reasoning
	}
	if turn.CachedInputTokens != nil {
		cached := *turn.CachedInputTokens
		if st.observed.CachedInputTokens != nil {
			cached = quota.AddBounded(cached, *st.observed.CachedInputTokens)
		}
		st.observed.CachedInputTokens = &cached
	}
	mergeUsageShare(&st.observed.AudioInputTokens, turn.AudioInputTokens)
	mergeUsageShare(&st.observed.CachedAudioInputTokens, turn.CachedAudioInputTokens)
	mergeUsageShare(&st.observed.AudioOutputTokens, turn.AudioOutputTokens)
	mergeUsageShare(&st.observed.ImageInputTokens, turn.ImageInputTokens)
	mergeUsageShare(&st.observed.CachedImageInputTokens, turn.CachedImageInputTokens)
	mergeUsageShare(&st.observed.ImageOutputTokens, turn.ImageOutputTokens)
	if len(turn.ToolCalls) > 0 {
		if st.observed.ToolCalls == nil {
			st.observed.ToolCalls = turn.ToolCalls
		} else {
			for tool, invocations := range turn.ToolCalls {
				st.observed.ToolCalls[tool] = quota.AddBounded(st.observed.ToolCalls[tool], invocations)
			}
			if st.observed.ToolCalls["google_search_prompt"] > 1 {
				st.observed.ToolCalls["google_search_prompt"] = 1
			}
		}
	}
	st.turn = nil
}

func (st *liveRelay) watch(done chan relayEnd) (relayEnd, int) {
	reauth := time.NewTicker(ingress.ReauthInterval)
	defer reauth.Stop()
	heartbeat := time.NewTicker(livePing)
	defer heartbeat.Stop()
	remaining := 2
	for {
		select {
		case first := <-done:
			remaining--
			return first, remaining
		case <-reauth.C:
			if end, ok := st.reauthorize(); !ok {
				return end, remaining
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(st.ctx, ingress.ReauthInterval)
			err := st.upstream.Ping(pingCtx)
			if err == nil {
				err = st.client.Ping(pingCtx)
			}
			pingCancel()
			if err != nil {
				return relayEnd{err: err}, remaining
			}
		case <-st.ctx.Done():
			return relayEnd{err: st.ctx.Err()}, remaining
		}
	}
}

func (st *liveRelay) reauthorize() (relayEnd, bool) {
	x := st.x
	authority, err := st.kernel.ReauthenticateSession(st.ctx, x, st.token)
	if err != nil || authority == nil || authority.ID != st.x.Authority().ID || !authority.Allows("inference", x.Route.Slug, x.Route.ProjectID, st.kernel.Now()) || !authority.AllowsEndUser(x.EndUserDigest()) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.EndUserDigest()) || !authority.AllowsAttribution(x.AttributionValues()) || !authority.AllowsClientIP(x.ClientIP()) || st.kernel.PinEligibility(st.p) != routing.Eligible {
		st.client.Close(websocket.StatusPolicyViolation, "authority revoked")
		return relayEnd{err: errLiveAuthority}, false
	}
	current := *authority
	current.EndUserDigest = x.EndUserDigest()
	if e := st.kernel.UnboundedPinLimits(x.AdmissionAuthority(current), st.p, x.Route); e != nil {
		st.client.Close(websocket.StatusPolicyViolation, "consumption budget changed")
		return relayEnd{err: errLiveAuthority}, false
	}
	return relayEnd{}, true
}

func (st *liveRelay) converge(first relayEnd, remaining int, done chan relayEnd) (*wire.Usage, bool, error) {
	st.mu.Lock()
	pending := st.inFlight
	st.mu.Unlock()
	code := websocket.CloseStatus(first.err)
	if st.x.Strict() && first.fromProvider && pending && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		first.err = fmt.Errorf("%w: %v", errLiveIncomplete, first.err)
		code = websocket.StatusInternalError
	}
	if st.x.Strict() && errors.Is(first.err, errLiveProviderError) {
		code = websocket.StatusInternalError
	}
	if code < 0 {
		code = websocket.StatusInternalError
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		st.upstream.Close(code, "")
		st.client.Close(code, "")
	} else {
		// An abruptly disconnected peer cannot finish a close handshake. Tear
		// down both transports so a blocked opposite writer releases its slot.
		st.upstream.CloseNow()
		st.client.CloseNow()
	}
	deliveryFailed := first.deliveryFailed
	for ; remaining > 0; remaining-- {
		end := <-done
		deliveryFailed = deliveryFailed || end.deliveryFailed
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.x.Strict() && !errors.Is(first.err, context.DeadlineExceeded) {
		if first.deliveryFailed || !first.fromProvider && first.sourceClosed && (st.inFlight || deliveryFailed) {
			return st.observed, false, fmt.Errorf("%w: %v", errLiveClientClosed, first.err)
		}
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		return st.observed, st.turn == nil && st.observed != nil, nil
	}
	return st.observed, false, first.err
}
