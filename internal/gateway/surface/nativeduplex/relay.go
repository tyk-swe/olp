package nativeduplex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/contracts/duplex"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/duplexrelay"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

func (s surface) duplexDial(ctx context.Context, x *kernel.Call, p *kernel.Pin, template *duplex.Template) (kernel.DuplexPeer, *fault.Error) {
	switch template.Transport() {
	case duplex.TransportBedrockBidi:
		return s.dialBedrockDuplex(ctx, x, p)
	case duplex.TransportWebSocket:
		endpoint, err := p.Provider.Connector().DuplexURL(p.Model)
		if err != nil {
			return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The duplex profile cannot address this model.")
		}
		check := strings.Replace(strings.Replace(endpoint, "wss://", "https://", 1), "ws://", "http://", 1)
		if parsed, parseErr := url.Parse(check); parseErr == nil {
			parsed.RawQuery = ""
			check = parsed.String()
		}
		if _, err := s.kernel.Egress.ValidateEndpoint(check); err != nil {
			return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The duplex provider address is not permitted.")
		}
		dialCtx, cancel := context.WithCancel(ctx)
		dialTimer := time.AfterFunc(p.Timeout(), cancel)
		conn, e := s.kernel.RealtimeDial(dialCtx, x, p, endpoint)
		dialTimer.Stop()
		cancel()
		if e != nil {
			return nil, e
		}
		limit := s.kernel.Config.MaxEventBytes
		if limit <= 0 {
			limit = 1 << 20
		}
		conn.SetReadLimit(limit)
		return conn, nil
	}
	return nil, fault.ServerError(http.StatusBadGateway, "upstream_error", "The duplex transport is unavailable.")
}

const (
	duplexPing = 30 * time.Second
)

type duplexRelay struct {
	surface
	x         *kernel.Call
	p         *kernel.Pin
	token     string
	observer  *duplex.Observer
	transport duplex.Transport
	maxBytes  int
}

func (s surface) relayNativeDuplex(ctx context.Context, x *kernel.Call, p *kernel.Pin, client, upstream kernel.DuplexPeer, token string, observer *duplex.Observer, transport duplex.Transport, maxBytes int) (*wire.Usage, bool, error) {
	st := &duplexRelay{surface: s, x: x, p: p, token: token, observer: observer, transport: transport, maxBytes: maxBytes}
	res := duplexrelay.Run(ctx, duplexrelay.Config{
		Name:                "duplex relay",
		Log:                 s.kernel.Log,
		Client:              client,
		Upstream:            upstream,
		PingInterval:        duplexPing,
		WriteTimeout:        duplexrelay.FrameWriteTimeout,
		OnClientFrame:       st.onClientFrame,
		OnProviderFrame:     st.onProviderFrame,
		OnProviderDelivered: func() { st.x.Delivered(st.kernel.Now()) },
		Reauthorize:         st.reauthorize,
		ResolveClose:        st.resolveClose,
	})
	return st.finalize(res)
}

func (st *duplexRelay) onClientFrame(kind websocket.MessageType, payload []byte) ([]byte, error) {
	if kind != websocket.MessageText {
		return nil, errDuplexClientProtocol
	}
	if err := st.observer.ClientFrame(payload); err != nil {
		return nil, fmt.Errorf("%w: %v", errDuplexClientProtocol, err)
	}
	return payload, nil
}

func (st *duplexRelay) onProviderFrame(kind websocket.MessageType, payload []byte) (duplexrelay.FrameAction, error) {
	if kind != websocket.MessageText {
		return duplexrelay.FrameAction{}, errDuplexProtocol
	}
	frame, err := st.observer.ServerFrame(payload)
	if err != nil {
		return duplexrelay.FrameAction{}, fmt.Errorf("%w: %v", errDuplexProtocol, err)
	}
	if !frame.ProviderError {
		return duplexrelay.FrameAction{Payload: payload}, nil
	}
	doc, parseErr := oif.ParseJSON(payload, oif.Limits{MaxBytes: st.maxBytes})
	if parseErr != nil {
		return duplexrelay.FrameAction{}, fmt.Errorf("%w: provider error could not be parsed", errDuplexProtocol)
	}
	scrubbed, redactErr := fault.RedactNativeFailureDocument(doc, st.x.Sensitive())
	if redactErr != nil {
		return duplexrelay.FrameAction{}, fmt.Errorf("%w: provider error could not be redacted", errDuplexProtocol)
	}
	return duplexrelay.FrameAction{Payload: scrubbed.Bytes(), StopErr: errDuplexProviderError}, nil
}

func (st *duplexRelay) reauthorize(ctx context.Context) (string, error) {
	check := st.kernel.CheckSessionAuthority(ctx, st.x, st.token, st.x.Authority().ID, st.p)
	switch check.Status {
	case kernel.SessionAuthorized:
		return "", nil
	case kernel.SessionBudgetChanged:
		return "consumption budget changed", errDuplexAuthority
	default:
		return "authority revoked", errDuplexAuthority
	}
}

func (st *duplexRelay) resolveClose(first duplexrelay.Result) (websocket.StatusCode, error) {
	err := first.FirstErr
	code := websocket.CloseStatus(err)
	if st.transport == duplex.TransportBedrockBidi && !first.ClientFailed && st.observer.PendingResponse() && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		err = fmt.Errorf("%w: %v", errDuplexIncomplete, err)
		code = websocket.StatusInternalError
	}
	if errors.Is(err, errDuplexProviderError) {
		code = websocket.StatusInternalError
	}
	if code < 0 {
		code = websocket.StatusInternalError
	}
	return code, err
}

func (st *duplexRelay) finalize(res duplexrelay.Result) (*wire.Usage, bool, error) {
	if st.transport == duplex.TransportBedrockBidi && res.ClientFailed && !errors.Is(res.FirstErr, context.Canceled) && !errors.Is(res.FirstErr, context.DeadlineExceeded) && (st.observer.PendingResponse() || res.DeliveryFailed) {
		return st.observer.Usage(), false, fmt.Errorf("%w: %v", errDuplexClientClosed, res.FirstErr)
	}
	if res.CloseCode == websocket.StatusNormalClosure || res.CloseCode == websocket.StatusGoingAway {
		return st.observer.Usage(), st.observer.Complete(), nil
	}
	return st.observer.Usage(), false, res.FirstErr
}
