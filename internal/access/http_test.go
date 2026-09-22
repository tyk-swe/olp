package access

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAccessBodyReadDeadline(t *testing.T) {
	for _, body := range []string{`{"email":`, `{}`} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			finished := make(chan error, 1)
			s := &Server{Origin: "https://console.test"}
			server := httptest.NewServer(s.Handle(func(r *http.Request) (Reply, error) {
				var body map[string]any
				err := Decode(r, &body)
				finished <- err
				return OK(body), err
			}))
			defer server.Close()
			conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// Leave the body open, even when the first JSON document is complete.
			if _, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: console.test\r\nOrigin: %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n%s", s.Origin, body); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("accepted an incomplete request body")
				}
			case <-time.After(18 * time.Second):
				t.Fatal("body read remained blocked after the 15-second access timeout")
			}
			if err = conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("incomplete body status: %d", resp.StatusCode)
			}
		})
	}
}

func TestHandleStreamProblemBeforeCommit(t *testing.T) {
	s := &Server{Origin: "https://console.test"}
	server := httptest.NewServer(s.HandleStream(1024, time.Minute, func(w http.ResponseWriter, r *http.Request) error {
		return Fail(http.StatusConflict, "media_job_busy", "The media job is being reconciled by another worker; retry shortly.")
	}))
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("pre-commit error status: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "media_job_busy") {
		t.Fatalf("pre-commit error body: %s", body)
	}
}

func TestHandleStreamErrorAfterCommit(t *testing.T) {
	s := &Server{Origin: "https://console.test"}
	server := httptest.NewServer(s.HandleStream(1024, time.Minute, func(w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprintf(w, "event: frame\ndata: {}\n\n"); err != nil {
			t.Error(err)
		}
		return Fail(http.StatusInternalServerError, "upstream_failed", "The provider stream failed.")
	}))
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-commit status: %d", resp.StatusCode)
	}
	if string(body) != "event: frame\ndata: {}\n\n" {
		t.Fatalf("post-commit body altered: %q", body)
	}
}

func TestHandleStreamResponseController(t *testing.T) {
	recorder := httptest.NewRecorder()
	tracker := &streamWriter{ResponseWriter: recorder}
	if tracker.Unwrap() != recorder {
		t.Fatal("Unwrap did not return the underlying writer")
	}
	if err := http.NewResponseController(tracker).Flush(); err != nil {
		t.Fatalf("flush through the tracker failed: %v", err)
	}
	if !recorder.Flushed {
		t.Fatal("flush did not reach the underlying writer")
	}
	s := &Server{Origin: "https://console.test"}
	const expected = "event: frame\ndata: {}\n\n"
	flushed := make(chan error, 1)
	release := make(chan struct{})
	server := httptest.NewServer(s.HandleStream(1024, time.Minute, func(w http.ResponseWriter, r *http.Request) error {
		if _, err := io.WriteString(w, expected); err != nil {
			flushed <- err
			return err
		}
		err := http.NewResponseController(w).Flush()
		flushed <- err
		if err != nil {
			return err
		}
		// The client must observe the frame while the handler remains open;
		// returning here would let net/http flush it implicitly.
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return nil
	}))
	defer server.Close()
	defer close(release)
	client := server.Client()
	client.Timeout = 2 * time.Second
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frame := make([]byte, len(expected))
	if _, err := io.ReadFull(resp.Body, frame); err != nil {
		t.Fatal(err)
	}
	if string(frame) != expected {
		t.Fatalf("flushed frame changed: %q", frame)
	}
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatalf("flush through the handler failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not report its flush result")
	}
}

func TestAccessBodyReadDeadlineAllowsKeepAlive(t *testing.T) {
	s := &Server{Origin: "https://console.test"}
	server := httptest.NewServer(s.Handle(func(r *http.Request) (Reply, error) {
		var body map[string]any
		err := Decode(r, &body)
		return OK(body), err
	}))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	for range 2 {
		if _, err = fmt.Fprintf(conn, "POST / HTTP/1.1\r\nHost: console.test\r\nOrigin: %s\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}", s.Origin); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "{}" {
			t.Fatalf("complete request: status=%d body=%s error=%v", resp.StatusCode, body, err)
		}
	}
}
