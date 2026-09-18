package process

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func serveListener(t *testing.T, handler http.Handler, age, drain time.Duration, limit int) (*listenerServer, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &listenerServer{handler: handler, requests: context.Background(), age: age, drain: drain, limit: limit}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return server, ln.Addr().String()
}

func TestHTTP2ConnectionAgeSendsGOAWAYAndPreservesAdmittedStream(t *testing.T) {
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	_, address := serveListener(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol %s", r.Proto)
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			io.WriteString(w, "completed")
		case <-r.Context().Done():
			cancelled <- struct{}{}
		}
	}), 200*time.Millisecond, 2*time.Second, 4)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http2.Transport{AllowHTTP: true}
	client, err := transport.NewClientConn(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req, _ := http.NewRequestWithContext(t.Context(), "GET", "http://"+address+"/", nil)
	response, err := client.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	deadline := time.After(2 * time.Second)
	// Go 1.27's x/net adapter only marks State.Closing after final close.
	// Admission availability changes as soon as the peer sends GOAWAY.
	for client.CanTakeNewRequest() {
		select {
		case <-deadline:
			t.Fatal("connection age did not send GOAWAY")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if client.CanTakeNewRequest() {
		t.Fatal("draining connection accepts new streams")
	}
	select {
	case <-cancelled:
		t.Fatal("age cancelled admitted stream")
	default:
	}
	close(release)
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "completed" {
		t.Fatalf("stream did not finish: %q, %v", body, err)
	}
}

func TestConnectionCapacityAndForcedShutdownAreBounded(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	server, address := serveListener(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}), time.Hour, time.Second, 1)
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example\r\n\r\n")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	overflow, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer overflow.Close()
	overflow.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	if _, err := overflow.Read(buf[:]); err == nil {
		t.Fatal("over-capacity connection was not closed")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("over-capacity connection queued")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("shutdown exceeded its budget")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("forced shutdown did not cancel request")
	}
}
