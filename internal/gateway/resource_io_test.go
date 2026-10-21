package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestResourceContentChecksFlushAndLength(t *testing.T) {
	response := httptest.NewRecorder()
	if written, err := copyResourceContent(response, strings.NewReader("abc"), 3); err != nil || written != 3 || !response.Flushed {
		t.Fatalf("valid content not flushed: %d %v", written, err)
	}
	failure := &pinnedFlushFailure{header: make(http.Header)}
	if written, err := copyResourceContent(failure, strings.NewReader("abc"), 3); written != 3 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("buffered flush failure was lost: %d %v", written, err)
	}
	short := httptest.NewRecorder()
	if _, err := copyResourceContent(short, strings.NewReader("ab"), 3); !errors.Is(err, io.ErrUnexpectedEOF) || short.Flushed {
		t.Fatalf("short content was committed as complete: %v", err)
	}
}
