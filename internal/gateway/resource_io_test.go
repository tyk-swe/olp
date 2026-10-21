package gateway

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResourceJSONReadHasSocketDeadline(t *testing.T) {
	app := &Server{cfg: Config{MaxBodyBytes: 1024}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, stop := context.WithTimeout(r.Context(), 50*time.Millisecond)
		defer stop()
		_, failure := app.readResourceBody(w, r.WithContext(ctx))
		if failure != nil {
			w.WriteHeader(failure.Status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := fmt.Fprintf(connection, "POST / HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 10\r\nConnection: close\r\n\r\n{", server.Listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("stalled body was not interrupted by its read deadline: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 400 || response.StatusCode >= 500 {
		t.Fatalf("stalled body status=%d", response.StatusCode)
	}
}
