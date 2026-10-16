//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type retainedBufferedFlushFailure struct {
	*httptest.ResponseRecorder
	flushes int
}

func (*retainedBufferedFlushFailure) SetWriteDeadline(time.Time) error { return nil }
func (w *retainedBufferedFlushFailure) FlushError() error {
	w.flushes++
	return io.ErrClosedPipe
}

func testRealtimePayloadDelivery(t *testing.T, send bool) {
	t.Helper()
	h := newAccessHarness(t)
	fixture := newStrictRealtimeFixture(t, "openai")
	slug, key := provisionStrictRealtime(t, h, "openai", fixture.URL+"/v1")
	sink := &captureSink{}
	h.Gateway.Sink = sink
	before := fixture.dials.Load()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, strings.Replace(h.HTTP.URL, "http://", "ws://", 1)+"/v1/realtime?model="+slug,
		&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	glEventually(t, "upstream realtime upgrade", func() bool { return fixture.dials.Load() > before })
	if send {
		if err = connection.Write(ctx, websocket.MessageText, strictRealtimeClientFrames[0]); err != nil {
			t.Fatal(err)
		}
		kind, frame, readErr := connection.Read(ctx)
		if readErr != nil || kind != websocket.MessageText || !bytes.Equal(frame, strictRealtimeProviderFrames[0]) {
			t.Fatalf("native session frame: %q %v", frame, readErr)
		}
	}
	// The provider waits for a client frame before producing any native output.
	// Closing without sending a frame leaves both sockets upgraded but silent.
	_ = connection.Close(websocket.StatusNormalClosure, "")
	glEventually(t, "silent realtime terminal envelope", func() bool { return sink.count() > 0 })
	envelope := sink.last()
	if len(envelope.Attempts) != 1 || !envelope.Committed {
		t.Fatalf("did not exercise the accepted native session: %+v", envelope)
	}
	if (envelope.FirstByte != nil) != send {
		t.Fatalf("payload delivery send=%t firstByte=%v", send, envelope.FirstByte)
	}
}

type retainedReplayWriteFailure struct {
	*httptest.ResponseRecorder
	failure  string
	deadline bool
}

func (w *retainedReplayWriteFailure) SetWriteDeadline(deadline time.Time) error {
	w.deadline = !deadline.IsZero()
	return nil
}
func (*retainedReplayWriteFailure) SetReadDeadline(time.Time) error { return nil }
func (w *retainedReplayWriteFailure) Write(data []byte) (int, error) {
	if !w.deadline || w.failure == "write" {
		return 0, io.ErrClosedPipe
	}
	if w.failure == "short" {
		return len(data) - 1, nil
	}
	return w.ResponseRecorder.Write(data)
}
func (w *retainedReplayWriteFailure) FlushError() error {
	if w.failure == "flush" {
		return io.ErrClosedPipe
	}
	w.ResponseRecorder.Flush()
	return nil
}
