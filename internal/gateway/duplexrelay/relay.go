package duplexrelay

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/platform/panics"
)

type Peer interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	Ping(context.Context) error
	Close(websocket.StatusCode, string) error
	CloseNow() error
}

type FrameSink interface {
	Write(context.Context, websocket.MessageType, []byte) error
}

// BoundedWriter keeps one watchdog per direction so every frame gets a fresh
// write window without allocating a per-frame context timer.
type BoundedWriter struct {
	dst     FrameSink
	parent  context.Context
	ctx     context.Context
	cancel  context.CancelFunc
	timeout time.Duration
	timer   *time.Timer
	expired atomic.Bool
}

func NewBoundedWriter(parent context.Context, dst FrameSink, timeout time.Duration) *BoundedWriter {
	if timeout <= 0 {
		timeout = fault.ResponseWriteTimeout
	}
	ctx, cancel := context.WithCancel(parent)
	w := &BoundedWriter{dst: dst, parent: parent, ctx: ctx, cancel: cancel, timeout: timeout}
	w.timer = time.AfterFunc(timeout, func() {
		w.expired.Store(true)
		w.cancel()
	})
	w.timer.Stop()
	return w
}

func (w *BoundedWriter) Close() {
	w.timer.Stop()
	w.cancel()
}

func (w *BoundedWriter) Write(typ websocket.MessageType, data []byte) error {
	if err := w.parent.Err(); err != nil {
		return err
	}
	w.timer.Reset(w.timeout)
	err := w.dst.Write(w.ctx, typ, data)
	if !w.timer.Stop() {
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

type FrameAction struct {
	Payload []byte
	StopErr error
}

type Result struct {
	FirstErr       error
	FromProvider   bool
	SourceClosed   bool
	ClientFailed   bool
	DeliveryFailed bool
	CloseCode      websocket.StatusCode
}

type Config struct {
	Name                    string
	Log                     *slog.Logger
	Client                  Peer
	Upstream                Peer
	PingInterval            time.Duration
	ReauthInterval          time.Duration
	WriteTimeout            time.Duration
	SendResolvedClientClose bool
	OnClientFrame           func(kind websocket.MessageType, data []byte) ([]byte, error)
	OnProviderFrame         func(kind websocket.MessageType, data []byte) (FrameAction, error)
	OnProviderDelivered     func()
	Reauthorize             func(ctx context.Context) (closeReason string, err error)
	ResolveClose            func(first Result) (websocket.StatusCode, error)
}

func Run(ctx context.Context, cfg Config) Result {
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan Result, 2)
	spawn := func(dst, src Peer, fromProvider bool) {
		defer panics.Recover(cfg.Log, cfg.Name, func(err error) { done <- Result{FirstErr: err, FromProvider: fromProvider} })
		done <- forward(relayCtx, cfg, dst, src, fromProvider)
	}
	go spawn(cfg.Upstream, cfg.Client, false)
	go spawn(cfg.Client, cfg.Upstream, true)

	first, remaining := watch(relayCtx, cfg, done)
	return converge(cfg, first, remaining, done, cancel)
}

func forward(ctx context.Context, cfg Config, dst, src Peer, fromProvider bool) Result {
	writer := NewBoundedWriter(ctx, dst, cfg.WriteTimeout)
	defer writer.Close()
	for {
		kind, data, err := src.Read(ctx)
		if err != nil {
			return Result{FirstErr: err, FromProvider: fromProvider, SourceClosed: true, ClientFailed: !fromProvider}
		}
		var stopErr error
		if fromProvider {
			if cfg.OnProviderFrame != nil {
				action, ferr := cfg.OnProviderFrame(kind, data)
				if ferr != nil {
					return Result{FirstErr: ferr, FromProvider: true}
				}
				data, stopErr = action.Payload, action.StopErr
			}
		} else if cfg.OnClientFrame != nil {
			next, ferr := cfg.OnClientFrame(kind, data)
			if ferr != nil {
				return Result{FirstErr: ferr, FromProvider: false}
			}
			data = next
		}
		if err := writer.Write(kind, data); err != nil {
			return Result{FirstErr: err, FromProvider: fromProvider, ClientFailed: fromProvider, DeliveryFailed: fromProvider}
		}
		if fromProvider {
			if cfg.OnProviderDelivered != nil {
				cfg.OnProviderDelivered()
			}
			if stopErr != nil {
				return Result{FirstErr: stopErr, FromProvider: true}
			}
		}
	}
}

func watch(ctx context.Context, cfg Config, done <-chan Result) (Result, int) {
	reauthInterval := cfg.ReauthInterval
	if reauthInterval <= 0 {
		reauthInterval = ingress.ReauthInterval
	}
	reauth := time.NewTicker(reauthInterval)
	defer reauth.Stop()

	var heartbeatC <-chan time.Time
	if cfg.PingInterval > 0 {
		heartbeat := time.NewTicker(cfg.PingInterval)
		defer heartbeat.Stop()
		heartbeatC = heartbeat.C
	}

	remaining := 2
	for {
		select {
		case end := <-done:
			return end, remaining - 1
		case <-reauth.C:
			if cfg.Reauthorize == nil {
				continue
			}
			if reason, err := cfg.Reauthorize(ctx); err != nil {
				_ = cfg.Upstream.CloseNow()
				_ = cfg.Client.Close(websocket.StatusPolicyViolation, reason)
				return Result{FirstErr: err, CloseCode: websocket.StatusPolicyViolation}, remaining
			}
		case <-heartbeatC:
			pingCtx, pingCancel := context.WithTimeout(ctx, reauthInterval)
			err := cfg.Upstream.Ping(pingCtx)
			clientFailed := false
			if err == nil {
				err = cfg.Client.Ping(pingCtx)
				clientFailed = err != nil
			}
			pingCancel()
			if err != nil {
				return Result{FirstErr: err, ClientFailed: clientFailed}, remaining
			}
		case <-ctx.Done():
			return Result{FirstErr: ctx.Err()}, remaining
		}
	}
}

func converge(cfg Config, first Result, remaining int, done <-chan Result, cancel context.CancelFunc) Result {
	code := first.CloseCode
	if cfg.ResolveClose != nil {
		code, first.FirstErr = cfg.ResolveClose(first)
	} else if code == 0 {
		code = websocket.CloseStatus(first.FirstErr)
		if code < 0 {
			code = websocket.StatusInternalError
		}
	}
	sendClientClose := first.CloseCode == 0 && cfg.SendResolvedClientClose && !first.ClientFailed
	if sendClientClose {
		_ = cfg.Client.Close(code, "")
	}
	cancel()
	graceful := first.CloseCode == 0 && (code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway)
	if graceful {
		_ = cfg.Upstream.Close(code, "")
	} else {
		_ = cfg.Upstream.CloseNow()
	}
	if !sendClientClose {
		if graceful {
			_ = cfg.Client.Close(code, "")
		} else {
			_ = cfg.Client.CloseNow()
		}
	}
	deliveryFailed := first.DeliveryFailed
	for ; remaining > 0; remaining-- {
		end := <-done
		deliveryFailed = deliveryFailed || end.DeliveryFailed
	}
	first.CloseCode = code
	first.DeliveryFailed = deliveryFailed
	return first
}
