package realtime

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/duplexrelay"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/gateway/realtimeresponses"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

type relayState struct {
	surface
	x         *kernel.Call
	p         *kernel.Pin
	token     string
	keyID     string
	usageMu   sync.Mutex
	usage     *wire.Usage
	have      bool
	observed  bool
	responses realtimeresponses.Tracker
}

func (s surface) relayRealtime(ctx context.Context, x *kernel.Call, p *kernel.Pin, client, conn kernel.DuplexPeer, token, keyID string) (*wire.Usage, bool, error) {
	st := &relayState{surface: s, x: x, p: p, token: token, keyID: keyID, usage: &wire.Usage{}}
	res := duplexrelay.Run(ctx, duplexrelay.Config{
		Name:                    "realtime relay",
		Log:                     s.kernel.Log,
		Client:                  client,
		Upstream:                conn,
		PingInterval:            realtimePing,
		WriteTimeout:            fault.ResponseWriteTimeout,
		SendResolvedClientClose: true,
		OnClientFrame:           st.onClientFrame,
		OnProviderFrame:         st.onProviderFrame,
		OnProviderDelivered:     st.onProviderDelivered,
		Reauthorize:             st.reauthorize,
		ResolveClose:            st.resolveClose,
	})
	return st.finalize(res)
}

func (st *relayState) onClientFrame(typ websocket.MessageType, data []byte) ([]byte, error) {
	if st.x.Strict() {
		st.usageMu.Lock()
		st.responses.ClientFrame(typ, data)
		st.usageMu.Unlock()
	}
	return data, nil
}

func (st *relayState) onProviderFrame(typ websocket.MessageType, data []byte) (duplexrelay.FrameAction, error) {
	st.observeProviderFrame(typ, data)
	if typ == websocket.MessageText && (bytes.Contains(data, []byte(`"error"`)) || bytes.Contains(data, []byte(`\`))) {
		scrubbed, err := redactRealtimeError(data, st.x.Sensitive())
		if err != nil {
			return duplexrelay.FrameAction{}, err
		}
		data = scrubbed
	}
	return duplexrelay.FrameAction{Payload: data}, nil
}

func (st *relayState) onProviderDelivered() {
	st.x.Delivered(st.kernel.Now())
	st.usageMu.Lock()
	st.observed = true
	st.usageMu.Unlock()
}

func (st *relayState) reauthorize(ctx context.Context) (string, error) {
	if !st.kernel.SessionStillAuthorized(ctx, st.x, st.token, st.keyID) {
		return "authority revoked", errRealtimeAuthorityRevoked
	}
	if eligibility := st.kernel.PinEligibility(st.p); eligibility != routing.Eligible {
		return "provider credential " + string(eligibility), &realtimeCredentialError{eligibility}
	}
	return "", nil
}

func (st *relayState) resolveClose(first duplexrelay.Result) (websocket.StatusCode, error) {
	code := first.CloseCode
	if code == 0 {
		code = websocket.CloseStatus(first.FirstErr)
		if code < 0 {
			code = websocket.StatusInternalError
		}
	}
	st.usageMu.Lock()
	pendingResponse := st.x.Strict() && st.responses.Pending()
	st.usageMu.Unlock()
	if !first.ClientFailed && pendingResponse && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		code = websocket.StatusInternalError
	}
	return code, first.FirstErr
}

func (st *relayState) finalize(res duplexrelay.Result) (*wire.Usage, bool, error) {
	st.usageMu.Lock()
	defer st.usageMu.Unlock()
	var observedUsage *wire.Usage
	if st.have {
		observedUsage = st.usage
	}
	pendingResponse := st.x.Strict() && st.responses.Pending()
	if res.ClientFailed && (pendingResponse || res.DeliveryFailed) {
		return observedUsage, st.observed, fmt.Errorf("%w: %v", errRealtimeClientClosed, res.FirstErr)
	}
	if !res.ClientFailed && pendingResponse && (websocket.CloseStatus(res.FirstErr) == websocket.StatusNormalClosure || websocket.CloseStatus(res.FirstErr) == websocket.StatusGoingAway) {
		return observedUsage, st.observed, fmt.Errorf("%w: %v", errRealtimeResponseIncomplete, res.FirstErr)
	}
	if res.CloseCode == websocket.StatusNormalClosure || res.CloseCode == websocket.StatusGoingAway {
		return observedUsage, st.observed, nil
	}
	if res.ClientFailed {
		return observedUsage, st.observed, fmt.Errorf("%w: %v", errRealtimeClientClosed, res.FirstErr)
	}
	if !st.have {
		return nil, st.observed, res.FirstErr
	}
	return st.usage, st.observed, res.FirstErr
}

func (st *relayState) observeProviderFrame(typ websocket.MessageType, data []byte) {
	st.usageMu.Lock()
	defer st.usageMu.Unlock()
	u := st.responses.ProviderFrame(typ, data, st.x.Strict())
	if u == nil {
		return
	}
	st.usage.InputTokens = quota.AddBounded(st.usage.InputTokens, u.InputTokens)
	st.usage.OutputTokens = quota.AddBounded(st.usage.OutputTokens, u.OutputTokens)
	addUsageShare(&st.usage.CachedInputTokens, u.CachedInputTokens)
	addUsageShare(&st.usage.AudioInputTokens, u.AudioInputTokens)
	addUsageShare(&st.usage.CachedAudioInputTokens, u.CachedAudioInputTokens)
	addUsageShare(&st.usage.AudioOutputTokens, u.AudioOutputTokens)
	addUsageShare(&st.usage.ImageInputTokens, u.ImageInputTokens)
	addUsageShare(&st.usage.CachedImageInputTokens, u.CachedImageInputTokens)
	st.have = true
}

func redactRealtimeError(data []byte, credentials egress.Sensitive) ([]byte, error) {
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: len(data) + 2048})
	if err != nil || doc.Root().Kind() != oif.Object {
		return []byte(credentials.Redact(string(data))), nil
	}
	isError := false
	if _, ok := doc.Root().Lookup("error"); ok {
		isError = true
	} else if kind, ok := doc.Root().Lookup("type"); ok {
		text, _ := kind.Text()
		isError = text == "error"
	}
	if !isError {
		return data, nil
	}
	redacted, err := fault.RedactNativeFailureDocument(doc, credentials)
	if err != nil {
		return nil, err
	}
	return redacted.Bytes(), nil
}
