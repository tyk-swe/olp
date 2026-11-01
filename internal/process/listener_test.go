package process

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
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

func TestManagementListenerFailsClosedWhenDelegationAuthenticationIsUnavailable(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://olp:olp@127.0.0.1:1/olp?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	mux := http.NewServeMux()
	Management{Access: &access.Server{Pool: pool, Auth: secrets.NewAuthKey(make([]byte, 32), "test")}, Runtime: &runtime.Manager{}, Egress: &egress.Policy{}}.Register(mux)
	_, address := serveListener(t, mux, time.Minute, time.Second, 4)
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/delegation-consents"},
		{http.MethodGet, "/api/v1/delegation-options"},
	} {
		t.Run(request.path, func(t *testing.T) {
			req, requestErr := http.NewRequestWithContext(t.Context(), request.method, "http://"+address+request.path, nil)
			if requestErr != nil {
				t.Fatal(requestErr)
			}
			response, responseErr := http.DefaultClient.Do(req)
			if responseErr != nil {
				t.Fatal(responseErr)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("delegation request with unavailable authentication returned %d", response.StatusCode)
			}
		})
	}
}
