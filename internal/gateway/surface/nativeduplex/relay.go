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
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/routing"
)

func (s surface) duplexDial(ctx context.Context, x *kernel.Call, p *kernel.Pin, template *duplexcontract.Template) (kernel.DuplexPeer, *fault.Error) {
	switch template.Transport() {
	case duplexcontract.TransportBedrockBidi:
		return s.kernel.BedrockDuplexDial(ctx, x, p)
	case duplexcontract.TransportWebSocket:
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

const duplexPing = 30 * time.Second

type relayEnd struct {
	err            error
	client         bool
	deliveryFailed bool
}

type duplexRelay struct {
	surface
	ctx       context.Context
	x         *kernel.Call
	p         *kernel.Pin
	client    kernel.DuplexPeer
	upstream  kernel.DuplexPeer
	token     string
	observer  *duplexcontract.Observer
	transport duplexcontract.Transport
	maxBytes  int
}

func (s surface) relayNativeDuplex(ctx context.Context, x *kernel.Call, p *kernel.Pin, client, upstream kernel.DuplexPeer, token string, observer *duplexcontract.Observer, transport duplexcontract.Transport, maxBytes int) (*wire.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st := &duplexRelay{surface: s, ctx: ctx, x: x, p: p, client: client, upstream: upstream, token: token, observer: observer, transport: transport, maxBytes: maxBytes}
	done := make(chan relayEnd, 2)
	relay := func(dst, src kernel.DuplexPeer, fromProvider bool) {
		defer panics.Recover(s.kernel.Log, "duplex relay", func(err error) { done <- relayEnd{err: err} })
		done <- st.forward(dst, src, fromProvider)
	}
	go relay(upstream, client, false)
	go relay(client, upstream, true)
	first, remaining := st.watch(done)
	cancel()
	return st.converge(first, remaining, done)
}

func (st *duplexRelay) forward(dst, src kernel.DuplexPeer, fromProvider bool) relayEnd {
	for {
		kind, payload, err := src.Read(st.ctx)
		if err != nil {
			return relayEnd{err: err, client: !fromProvider}
		}
		if kind != websocket.MessageText {
			if fromProvider {
				return relayEnd{err: errDuplexProtocol}
			}
			return relayEnd{err: errDuplexClientProtocol}
		}
		providerError, end, handled := st.observe(payload, fromProvider)
		if handled {
			return end
		}
		payload, frmErr := st.frame(payload, providerError, fromProvider)
		if frmErr != nil {
			return relayEnd{err: frmErr}
		}
		if err := kernel.WriteDuplexFrame(st.ctx, dst, kind, payload); err != nil {
			return relayEnd{err: err, client: fromProvider, deliveryFailed: fromProvider}
		}
		if fromProvider {
			st.x.Delivered(st.kernel.Now())
			if providerError {
				return relayEnd{err: errDuplexProviderError}
			}
		}
	}
}

func (st *duplexRelay) observe(payload []byte, fromProvider bool) (bool, relayEnd, bool) {
	if !fromProvider {
		if err := st.observer.ClientFrame(payload); err != nil {
			return false, relayEnd{err: fmt.Errorf("%w: %v", errDuplexClientProtocol, err)}, true
		}
		return false, relayEnd{}, false
	}
	frame, observeErr := st.observer.ServerFrame(payload)
	if observeErr != nil {
		return false, relayEnd{err: fmt.Errorf("%w: %v", errDuplexProtocol, observeErr)}, true
	}
	return frame.ProviderError, relayEnd{}, false
}

func (st *duplexRelay) frame(payload []byte, providerError, fromProvider bool) ([]byte, error) {
	if !fromProvider || !providerError {
		return payload, nil
	}
	doc, parseErr := oif.ParseJSON(payload, oif.Limits{MaxBytes: st.maxBytes})
	if parseErr != nil {
		return nil, fmt.Errorf("%w: provider error could not be parsed", errDuplexProtocol)
	}
	scrubbed, redactErr := fault.RedactNativeFailureDocument(doc, st.x.Sensitive())
	if redactErr != nil {
		return nil, fmt.Errorf("%w: provider error could not be redacted", errDuplexProtocol)
	}
	return scrubbed.Bytes(), nil
}

func (st *duplexRelay) watch(done chan relayEnd) (relayEnd, int) {
	reauth := time.NewTicker(ingress.ReauthInterval)
	defer reauth.Stop()
	heartbeat := time.NewTicker(duplexPing)
	defer heartbeat.Stop()
	remaining := 2
	for {
		select {
		case end := <-done:
			remaining--
			return end, remaining
		case <-reauth.C:
			if end, ok := st.reauthorize(); !ok {
				return end, remaining
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(st.ctx, ingress.ReauthInterval)
			err := st.upstream.Ping(pingCtx)
			clientFailed := false
			if err == nil {
				err = st.client.Ping(pingCtx)
				clientFailed = err != nil
			}
			pingCancel()
			if err != nil {
				return relayEnd{err: err, client: clientFailed}, remaining
			}
		case <-st.ctx.Done():
			return relayEnd{err: st.ctx.Err()}, remaining
		}
	}
}

func (st *duplexRelay) reauthorize() (relayEnd, bool) {
	authority, err := st.kernel.ReauthenticateSession(st.ctx, st.x, st.token)
	x := st.x
	if err != nil || authority == nil || authority.ID != st.x.Authority().ID || !authority.Allows("inference", x.Route.Slug, x.Route.ProjectID, st.kernel.Now()) || !authority.AllowsEndUser(x.EndUserDigest()) || (authority.WorkloadIssuerID != nil && authority.EndUserDigest != x.EndUserDigest()) || !authority.AllowsAttribution(x.AttributionValues()) || !authority.AllowsClientIP(x.ClientIP()) || st.kernel.PinEligibility(st.p) != routing.Eligible {
		st.upstream.CloseNow()
		st.client.Close(websocket.StatusPolicyViolation, "authority revoked")
		return relayEnd{err: errDuplexAuthority}, false
	}
	current := *authority
	current.EndUserDigest = x.EndUserDigest()
	if e := st.kernel.UnboundedPinLimits(x.AdmissionAuthority(current), st.p, x.Route); e != nil {
		st.upstream.CloseNow()
		st.client.Close(websocket.StatusPolicyViolation, "consumption budget changed")
		return relayEnd{err: errDuplexAuthority}, false
	}
	return relayEnd{}, true
}

func (st *duplexRelay) converge(first relayEnd, remaining int, done chan relayEnd) (*wire.Usage, bool, error) {
	code := websocket.CloseStatus(first.err)
	if st.transport == duplexcontract.TransportBedrockBidi && !first.client && st.observer.PendingResponse() && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		first.err = fmt.Errorf("%w: %v", errDuplexIncomplete, first.err)
		code = websocket.StatusInternalError
	}
	if errors.Is(first.err, errDuplexProviderError) {
		code = websocket.StatusInternalError
	}
	if code < 0 {
		code = websocket.StatusInternalError
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		st.upstream.Close(code, "")
		st.client.Close(code, "")
	} else {
		st.upstream.CloseNow()
		st.client.CloseNow()
	}
	deliveryFailed := first.deliveryFailed
	for ; remaining > 0; remaining-- {
		end := <-done
		deliveryFailed = deliveryFailed || end.deliveryFailed
	}
	if st.transport == duplexcontract.TransportBedrockBidi && first.client && !errors.Is(first.err, context.Canceled) && !errors.Is(first.err, context.DeadlineExceeded) && (st.observer.PendingResponse() || deliveryFailed) {
		return st.observer.Usage(), false, fmt.Errorf("%w: %v", errDuplexClientClosed, first.err)
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		return st.observer.Usage(), st.observer.Complete(), nil
	}
	return st.observer.Usage(), false, first.err
}
