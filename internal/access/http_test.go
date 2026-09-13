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
			server := httptest.NewServer(s.handle(func(r *http.Request) (reply, error) {
				var body map[string]any
				err := decode(r, &body)
				finished <- err
				return ok(body), err
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

func TestAccessBodyReadDeadlineAllowsKeepAlive(t *testing.T) {
	s := &Server{Origin: "https://console.test"}
	server := httptest.NewServer(s.handle(func(r *http.Request) (reply, error) {
		var body map[string]any
		err := decode(r, &body)
		return ok(body), err
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
