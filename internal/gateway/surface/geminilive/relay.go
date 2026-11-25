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
	"github.com/tyk-swe/olp/internal/gateway/duplexrelay"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
)

const liveFrameWriteTimeout = 5 * time.Second

type liveRelay struct {
	surface
	x        *kernel.Call
	p        *kernel.Pin
	token    string
	maxBytes int
	mu       sync.Mutex
	observed *wire.Usage
	turn     *wire.Usage
	ground   int64
	inFlight bool
}

func (s surface) relayLive(ctx context.Context, x *kernel.Call, p *kernel.Pin, client, upstream *websocket.Conn, token string, maxBytes int) (*wire.Usage, bool, error) {
	st := &liveRelay{surface: s, x: x, p: p, token: token, maxBytes: maxBytes}
	res := duplexrelay.Run(ctx, duplexrelay.Config{
		Name:                "gemini live relay",
		Log:                 s.kernel.Log,
		Client:              client,
		Upstream:            upstream,
		PingInterval:        livePing,
		WriteTimeout:        liveFrameWriteTimeout,
		OnClientFrame:       st.onClientFrame,
		OnProviderFrame:     st.onProviderFrame,
		OnProviderDelivered: func() { st.x.Delivered(st.kernel.Now()) },
		Reauthorize:         st.reauthorize,
		ResolveClose:        st.resolveClose,
	})
	return st.finalize(res)
}

func (st *liveRelay) onClientFrame(kind websocket.MessageType, payload []byte) ([]byte, error) {
	if kind != websocket.MessageText {
		return nil, errLiveClientProtocol
	}
	if err := geminilifecycle.ValidateLiveClientFrame(payload, st.maxBytes); err != nil {
		return nil, fmt.Errorf("%w: %v", errLiveClientProtocol, err)
	}
	if st.x.Strict() {
		st.mu.Lock()
		st.inFlight = true
		st.mu.Unlock()
	}
	return payload, nil
}

func (st *liveRelay) onProviderFrame(kind websocket.MessageType, payload []byte) (duplexrelay.FrameAction, error) {
	if kind != websocket.MessageText {
		return duplexrelay.FrameAction{}, errLiveProtocol
	}
	out, providerError, err := st.providerPayload(payload)
	if err != nil {
		return duplexrelay.FrameAction{}, err
	}
	var stopErr error
	if st.x.Strict() && providerError {
		stopErr = errLiveProviderError
	}
	return duplexrelay.FrameAction{Payload: out, StopErr: stopErr}, nil
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
	var groundQueries int64
	if frame.Kind == "serverContent" {
		if content, ok := doc.Lookup("serverContent"); ok {
			if grounding, ok := content.Lookup("groundingMetadata"); ok {
				if queries, ok := grounding.Lookup("webSearchQueries"); ok && queries.Kind() == oif.Array {
					groundQueries = int64(len(queries.Elements()))
				}
			}
		}
	}
	out := payload
	if frame.Kind == "error" {
		scrubbed, err := fault.RedactNativeFailureDocument(doc, st.x.Sensitive())
		if err != nil {
			return nil, false, fmt.Errorf("%w: provider error could not be redacted", errLiveProtocol)
		}
		out = scrubbed.Bytes()
	}
	if groundQueries > 0 || (st.x.Strict() && (frame.Kind == "serverContent" || frame.Kind == "toolCall")) {
		st.mu.Lock()
		st.ground += groundQueries
		if st.x.Strict() && (frame.Kind == "serverContent" || frame.Kind == "toolCall") {
			st.inFlight = !frame.TurnComplete
		}
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

func (st *liveRelay) reauthorize(ctx context.Context) (string, error) {
	check := st.kernel.CheckSessionAuthority(ctx, st.x, st.token, st.x.Authority().ID, st.p)
	switch check.Status {
	case kernel.SessionAuthorized:
		return "", nil
	case kernel.SessionBudgetChanged:
		return "consumption budget changed", errLiveAuthority
	default:
		return "authority revoked", errLiveAuthority
	}
}

func (st *liveRelay) resolveClose(first duplexrelay.Result) (websocket.StatusCode, error) {
	st.mu.Lock()
	pending := st.inFlight
	st.mu.Unlock()
	err := first.FirstErr
	code := websocket.CloseStatus(err)
	if st.x.Strict() && first.FromProvider && pending && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		err = fmt.Errorf("%w: %v", errLiveIncomplete, err)
		code = websocket.StatusInternalError
	}
	if st.x.Strict() && errors.Is(err, errLiveProviderError) {
		code = websocket.StatusInternalError
	}
	if code < 0 {
		code = websocket.StatusInternalError
	}
	return code, err
}

func (st *liveRelay) finalize(res duplexrelay.Result) (*wire.Usage, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.x.Strict() && !errors.Is(res.FirstErr, context.DeadlineExceeded) {
		if res.ClientFailed && (res.FromProvider || res.SourceClosed && (st.inFlight || res.DeliveryFailed)) {
			return st.observed, false, fmt.Errorf("%w: %v", errLiveClientClosed, res.FirstErr)
		}
	}
	if res.CloseCode == websocket.StatusNormalClosure || res.CloseCode == websocket.StatusGoingAway {
		return st.observed, st.turn == nil && st.observed != nil, nil
	}
	return st.observed, false, res.FirstErr
}
