package duplexrelay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakePeer struct {
	reads         chan readResult
	writes        chan []byte
	blockClose    bool
	closedNormal  atomic.Bool
	closedNow     atomic.Bool
	closeCode     atomic.Int32
	writeBlockCtx bool
}

type readResult struct {
	kind websocket.MessageType
	data []byte
	err  error
}

func newFakePeer() *fakePeer {
	return &fakePeer{reads: make(chan readResult, 4), writes: make(chan []byte, 4)}
}

func (p *fakePeer) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case msg := <-p.reads:
		return msg.kind, msg.data, msg.err
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (p *fakePeer) Write(ctx context.Context, _ websocket.MessageType, data []byte) error {
	if p.writeBlockCtx {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case p.writes <- append([]byte(nil), data...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *fakePeer) Ping(context.Context) error { return nil }

func (p *fakePeer) Close(code websocket.StatusCode, _ string) error {
	p.closeCode.Store(int32(code))
	p.closedNormal.Store(true)
	if p.blockClose {
		select {}
	}
	return nil
}

func (p *fakePeer) CloseNow() error {
	p.closedNow.Store(true)
	return nil
}

func TestRunUsesCloseNowOnAbnormalFailure(t *testing.T) {
	t.Parallel()
	client := newFakePeer()
	upstream := newFakePeer()
	client.blockClose = true
	upstream.blockClose = true
	upstream.reads <- readResult{err: errors.New("broken pipe")}

	res := Run(t.Context(), Config{
		Name:     "test",
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Client:   client,
		Upstream: upstream,
	})
	if res.FirstErr == nil || !client.closedNow.Load() || !upstream.closedNow.Load() {
		t.Fatalf("abnormal relay did not call CloseNow: res=%+v clientNow=%v upstreamNow=%v", res, client.closedNow.Load(), upstream.closedNow.Load())
	}
}

func TestRunSendsResolvedErrorCloseToClient(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	upstream := newFakePeer()
	finished := make(chan Result, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept WebSocket: %v", err)
			return
		}
		defer client.CloseNow()
		finished <- Run(ctx, Config{
			Name: "test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Client: client, Upstream: upstream,
			SendResolvedClientClose: true,
			ResolveClose: func(first Result) (websocket.StatusCode, error) {
				return websocket.StatusInternalError, first.FirstErr
			},
		})
	}))
	defer server.Close()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	if err := client.Write(ctx, websocket.MessageText, []byte("response.create")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstream.writes:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	upstream.reads <- readResult{err: websocket.CloseError{Code: websocket.StatusNormalClosure}}
	_, _, err = client.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Fatalf("client close = %v (%v), want %v", got, err, websocket.StatusInternalError)
	}
	select {
	case result := <-finished:
		if result.CloseCode != websocket.StatusInternalError {
			t.Fatalf("relay close = %v, want %v", result.CloseCode, websocket.StatusInternalError)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestRunStopsOnReauthorizeFailure(t *testing.T) {
	t.Parallel()
	client := newFakePeer()
	upstream := newFakePeer()
	wantErr := errors.New("budget changed")

	res := Run(t.Context(), Config{
		Name:           "test",
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Client:         client,
		Upstream:       upstream,
		ReauthInterval: 10 * time.Millisecond,
		Reauthorize: func(context.Context) (string, error) {
			return "consumption budget changed", wantErr
		},
	})
	if !errors.Is(res.FirstErr, wantErr) || res.CloseCode != websocket.StatusPolicyViolation {
		t.Fatalf("reauth failure = %+v", res)
	}
	if !upstream.closedNow.Load() || websocket.StatusCode(client.closeCode.Load()) != websocket.StatusPolicyViolation {
		t.Fatalf("expected upstream CloseNow and client policy violation close")
	}
}

func TestBoundedWriterTimesOutStalledPeer(t *testing.T) {
	t.Parallel()
	stalled := newFakePeer()
	stalled.writeBlockCtx = true
	w := NewBoundedWriter(t.Context(), stalled, 20*time.Millisecond)
	defer w.Close()
	if err := w.Write(websocket.MessageText, []byte("hello")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}
