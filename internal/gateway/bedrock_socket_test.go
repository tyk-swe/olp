package gateway

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

func TestBedrockNativeStreamUnblocksAStalledClient(t *testing.T) {
	h, x, p := pinnedExchangeFixture(t)
	x.family = wire.BedrockInvoke
	h.gateway.cfg.MaxEventBytes = 2 << 20
	e := h.gateway.beginPinned(context.Background(), x, p, http.MethodPost)
	e.state.accepted, x.dispatched = true, true
	e.state.dispatched.Store(true)
	var upstream bytes.Buffer
	encoder := eventstream.NewEncoder()
	for _, message := range []*eventstream.Message{
		bedrockChunk(t, `{"type":"message_start","message":{"usage":{"input_tokens":11,"output_tokens":1}}}`),
		bedrockChunk(t, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"`+strings.Repeat("x", 1<<20)+`"}}`),
	} {
		if err := encoder.Encode(&upstream, *message); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.gateway.relayBedrockStream(r.Context(), socketDeadlineWriter{w, t}, x, p,
			&http.Response{Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(upstream.Bytes()))})
		close(done)
	}))
	server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetWriteBuffer(1024)
		}
		return ctx
	}
	server.Start()
	t.Cleanup(server.Close)
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Close the client before joining server cleanup, including on a regression.
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(socketTestGuard):
		t.Fatal("native relay retained its upstream work behind a stalled client")
	}
	fact := x.facts[0]
	if x.failure == nil || x.failure.Code != "client_delivery_failed" || fact.Class != classCancelled {
		t.Fatalf("stalled client disposition: failure=%v fact=%+v", x.failure, fact)
	}
	if fact.Usage == nil || fact.UsageComplete || !fact.BillingUncertain {
		t.Fatalf("partial provider usage was lost or settled: %+v", fact)
	}
}
