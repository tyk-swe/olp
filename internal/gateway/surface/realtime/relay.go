package realtime

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/gateway/realtimeresponses"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/routing"
)

type relayEnd struct {
	err            error
	client         bool
	deliveryFailed bool
}

type relayState struct {
	surface
	ctx       context.Context
	x         *kernel.Call
	p         *kernel.Pin
	client    kernel.DuplexPeer
	conn      kernel.DuplexPeer
	token     string
	keyID     string
	usageMu   sync.Mutex
	usage     *wire.Usage
	have      bool
	observed  bool
	responses realtimeresponses.Tracker
}

func (s surface) relayRealtime(ctx context.Context, x *kernel.Call, p *kernel.Pin, client, conn kernel.DuplexPeer, token, keyID string) (*wire.Usage, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st := &relayState{surface: s, ctx: ctx, x: x, p: p, client: client, conn: conn, token: token, keyID: keyID, usage: &wire.Usage{}}
	done := make(chan relayEnd, 2)
	relay := func(dst, src kernel.DuplexPeer, inspect bool) {
		defer panics.Recover(s.kernel.Log, "realtime relay", func(err error) { done <- relayEnd{err: err} })
		done <- st.forward(dst, src, inspect)
	}
	go relay(conn, client, false)
	go relay(client, conn, true)
	first, clientClosed, deliveryFailed, code, remaining := st.watch(done)
	cancel()
	return st.converge(first, clientClosed, deliveryFailed, code, remaining, done)
}

func (st *relayState) forward(dst, src kernel.DuplexPeer, inspect bool) relayEnd {
	writer := newRealtimeBoundedWriter(st.ctx, dst, fault.ResponseWriteTimeout)
	defer writer.close()
	for {
		typ, data, err := src.Read(st.ctx)
		if err != nil {
			return relayEnd{err: err, client: !inspect}
		}
		if inspect {
			st.observeProviderFrame(typ, data)
		} else if st.x.Strict() {
			st.usageMu.Lock()
			st.responses.ClientFrame(typ, data)
			st.usageMu.Unlock()
		}
		if inspect && typ == websocket.MessageText &&
			(bytes.Contains(data, []byte(`"error"`)) || bytes.Contains(data, []byte(`\`))) {
			// An in-band provider error can echo an applied credential;
			// scrub it before the frame reaches the client. The gate also
			// admits any frame carrying a JSON escape, since a member or
			// value spelled like "err\u006fr" hides the marker.
			scrubbed, err := redactRealtimeError(data, st.x.Sensitive())
			if err != nil {
				return relayEnd{err: err}
			}
			data = scrubbed
		}
		if err := writer.write(typ, data); err != nil {
			return relayEnd{err: err, client: inspect, deliveryFailed: inspect}
		}
		if inspect {
			st.x.Delivered(st.kernel.Now())
			st.observed = true
		}
	}
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

func (st *relayState) watch(done chan relayEnd) (error, bool, bool, websocket.StatusCode, int) {
	reauth := time.NewTicker(ingress.ReauthInterval)
	defer reauth.Stop()
	heartbeat := time.NewTicker(realtimePing)
	defer heartbeat.Stop()
	remaining := 2
	for {
		select {
		case end := <-done:
			remaining--
			return end.err, end.client, end.deliveryFailed, 0, remaining
		case <-reauth.C:
			if !st.kernel.SessionStillAuthorized(st.ctx, st.x, st.token, st.keyID) {
				st.client.Close(websocket.StatusPolicyViolation, "authority revoked")
				return errRealtimeAuthorityRevoked, false, false, websocket.StatusPolicyViolation, remaining
			}
			if eligibility := st.kernel.PinEligibility(st.p); eligibility != routing.Eligible {
				st.client.Close(websocket.StatusPolicyViolation, "provider credential "+string(eligibility))
				return &realtimeCredentialError{eligibility}, false, false, websocket.StatusPolicyViolation, remaining
			}
		case <-heartbeat.C:
			pingCtx, pingCancel := context.WithTimeout(st.ctx, ingress.ReauthInterval)
			err := st.conn.Ping(pingCtx)
			if err == nil {
				err = st.client.Ping(pingCtx)
			}
			pingCancel()
			if err != nil {
				return err, false, false, 0, remaining
			}
		case <-st.ctx.Done():
			return st.ctx.Err(), false, false, 0, remaining
		}
	}
}

func (st *relayState) converge(first error, clientClosed, clientDeliveryFailed bool, closeCode websocket.StatusCode, remaining int, done chan relayEnd) (*wire.Usage, bool, error) {
	code := websocket.CloseStatus(first)
	if closeCode != 0 {
		code = closeCode
	} else if code < 0 {
		code = websocket.StatusInternalError
	}
	st.usageMu.Lock()
	pending := st.x.Strict() && st.responses.Pending()
	st.usageMu.Unlock()
	if !clientClosed && pending && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway) {
		// The provider closed a socket while a native response still owed
		// its response.done terminal event. Never mirror its 1000/1001 as
		// a successful client-visible session end.
		code = websocket.StatusInternalError
	}
	st.conn.Close(code, "")
	st.client.Close(code, "")
	// A control event has consumed neither result; a forwarding result has
	// consumed one. Join every remaining task before final usage/delivery state.
	for ; remaining > 0; remaining-- {
		end := <-done
		clientDeliveryFailed = clientDeliveryFailed || end.deliveryFailed
	}
	st.usageMu.Lock()
	defer st.usageMu.Unlock()
	var observedUsage *wire.Usage
	if st.have {
		observedUsage = st.usage
	}
	pending = st.x.Strict() && st.responses.Pending()
	if clientClosed && (pending || clientDeliveryFailed) {
		return observedUsage, st.observed, fmt.Errorf("%w: %v", errRealtimeClientClosed, first)
	}
	if !clientClosed && pending && (websocket.CloseStatus(first) == websocket.StatusNormalClosure || websocket.CloseStatus(first) == websocket.StatusGoingAway) {
		return observedUsage, st.observed, fmt.Errorf("%w: %v", errRealtimeResponseIncomplete, first)
	}
	if code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway {
		return observedUsage, st.observed, nil
	}
	if clientClosed {
		return observedUsage, st.observed, fmt.Errorf("%w: %v", errRealtimeClientClosed, first)
	}
	if !st.have {
		return nil, st.observed, first
	}
	return st.usage, st.observed, first
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

// realtimeBoundedWriter keeps one watchdog per direction. Every frame gets a
// fresh full write window without a new context timer for each frame. The
// WebSocket's own cancellation hook still closes a stalled connection and
// interrupts a writer waiting on its internal lock.
type realtimeFrameSink interface {
	Write(context.Context, websocket.MessageType, []byte) error
}

type realtimeBoundedWriter struct {
	dst     realtimeFrameSink
	parent  context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	expired atomic.Bool
}

func newRealtimeBoundedWriter(parent context.Context, dst realtimeFrameSink, timeout time.Duration) *realtimeBoundedWriter {
	w := &realtimeBoundedWriter{dst: dst, parent: parent, timeout: timeout}
	w.ctx, w.cancel = context.WithCancel(parent)
	w.timer = time.AfterFunc(timeout, func() {
		w.expired.Store(true)
		w.cancel()
	})
	w.timer.Stop()
	return w
}

func (w *realtimeBoundedWriter) close() {
	w.timer.Stop()
	w.cancel()
}

func (w *realtimeBoundedWriter) write(typ websocket.MessageType, data []byte) error {
	if err := w.parent.Err(); err != nil {
		return err
	}
	w.timer.Reset(w.timeout)
	err := w.dst.Write(w.ctx, typ, data)
	if !w.timer.Stop() {
		// The deadline won a race with successful completion. Conservatively
		// terminate this direction; it cannot start another bounded write.
		w.expired.Store(true)
		w.cancel()
	}
	if w.expired.Load() {
		return context.DeadlineExceeded
	}
	if parentErr := w.parent.Err(); parentErr != nil {
		return parentErr
	}
	return err
}
