package process

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
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

func TestHijackedConnectionOutlivesUpgrade(t *testing.T) {
	wrote := make(chan error, 1)
	_, address := serveListener(t, hijackingHandler(t, wrote, nil), time.Hour, time.Second, 4)
	conn := upgrade(t, address)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var received strings.Builder
	buf := make([]byte, 256)
	for !strings.HasSuffix(received.String(), "frame") {
		n, err := conn.Read(buf)
		received.Write(buf[:n])
		if err != nil {
			t.Fatalf("read %q: %v", received.String(), err)
		}
	}
	if err := <-wrote; err != nil {
		t.Fatalf("handler write: %v", err)
	}
}

func TestHijackedConnectionCountsAgainstLimit(t *testing.T) {
	wrote := make(chan error, 2)
	held := make(chan net.Conn, 2)
	_, address := serveListener(t, hijackingHandler(t, wrote, held), time.Hour, time.Second, 1)
	first := upgrade(t, address)
	<-held
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
	<-wrote
	first.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		next := upgrade(t, address)
		select {
		case <-held:
			return
		case <-time.After(100 * time.Millisecond):
		}
		next.Close()
		if time.Now().After(deadline) {
			t.Fatal("capacity was not released after the hijacked connection closed")
		}
	}
}

func TestShutdownClosesHijackedConnections(t *testing.T) {
	read := make(chan error, 1)
	server, address := serveListener(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		var buf [1]byte
		_, err = conn.Read(buf[:])
		read <- err
	}), time.Hour, time.Second, 4)
	upgrade(t, address)
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		hijacked := false
		for entry := range server.connections {
			hijacked = hijacked || entry.hijacked.Load()
		}
		server.mu.Unlock()
		if hijacked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("connection was not hijacked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("hijacked read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown left the hijacked connection open")
	}
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

func TestListenerRetriesTemporaryAcceptErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	emfile := &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	server := &listenerServer{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), requests: context.Background(), age: time.Hour, drain: time.Second, limit: 4}
	done := make(chan error, 1)
	go func() { done <- server.Serve(&flakyListener{Listener: ln, failures: 2, err: emfile}) }()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	select {
	case err := <-done:
		t.Fatalf("serve returned: %v", err)
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	server.Shutdown(ctx)
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serve: %v", err)
	}
}

func TestListenerStopsOnPermanentAcceptErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	boom := errors.New("boom")
	server := &listenerServer{handler: http.NotFoundHandler(), requests: context.Background(), age: time.Hour, drain: time.Second, limit: 4}
	if err := server.Serve(&flakyListener{Listener: ln, failures: 1, err: boom}); !errors.Is(err, boom) {
		t.Fatalf("serve: %v", err)
	}
}
