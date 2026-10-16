package process

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
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

// Go 1.27's x/net adapter only marks State.Closing after final close.
// Admission availability changes as soon as the peer sends GOAWAY.

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

// hijackingHandler upgrades the connection, waits, writes a frame and holds
// the socket until the client closes it.
func hijackingHandler(t *testing.T, wrote chan<- error, held chan<- net.Conn) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		if held != nil {
			held <- conn
		}
		time.Sleep(200 * time.Millisecond)
		_, err = io.WriteString(conn, "frame")
		wrote <- err
		io.Copy(io.Discard, conn)
		conn.Close()
	})
}

func upgrade(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
	return conn
}

type flakyListener struct {
	net.Listener
	failures int
	err      error
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.failures > 0 {
		l.failures--
		return nil, l.err
	}
	return l.Listener.Accept()
}
