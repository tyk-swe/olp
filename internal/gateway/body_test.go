package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestIncompleteBodiesReleaseAdmission(t *testing.T) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write([]byte(`{"model":"team-chat"}`)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, encoding, body string
	}{
		{"partial JSON", "identity", `{"model":`},
		{"complete JSON with incomplete HTTP body", "identity", `{}`},
		{"partial gzip header", "gzip", "\x1f\x8b"},
		{"partial gzip body", "gzip", string(compressed.Bytes()[:compressed.Len()-8])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{MaxInFlight: 1, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096})
			conn, err := net.DialTimeout("tcp", h.server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(requestBodyTimeout + 3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: gateway.test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Encoding: %s\r\nContent-Length: %d\r\n\r\n%s", fullKey, tc.encoding, len(tc.body)+1000, tc.body); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("unfinished upload did not receive a bounded response: %v", err)
			}
			defer resp.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusRequestTimeout || errorCode(t, body) != "request_timeout" {
				t.Fatalf("status %d body %v", resp.StatusCode, body)
			}
			if h.mock.count("a") != 0 || h.mock.count("b") != 0 || h.gateway.admission.Admitted() != 0 {
				t.Fatal("unfinished upload dispatched upstream or retained admission")
			}
			if resp, body := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
				t.Fatalf("admission did not recover: %d %v", resp.StatusCode, body)
			}
		})
	}
}

func TestCompletedUploadDoesNotLimitInferenceOrKeepAlive(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{})
	route := h.rt.release.Snapshot.Routes[routeSlug]
	route.OverallTimeout = (requestBodyTimeout + 5*time.Second).Milliseconds()
	route.Targets[0].Timeout = route.OverallTimeout
	h.rt.release.Snapshot.Routes[routeSlug] = route
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(requestBodyTimeout + 100*time.Millisecond):
			completion(modelA, answerText)(w, r)
		case <-r.Context().Done():
		}
	})
	conn, err := net.DialTimeout("tcp", h.server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(requestBodyTimeout + 5*time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}]}`
	requests := []string{
		fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: gateway.test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", fullKey, len(body), body),
		fmt.Sprintf("GET /v1/models HTTP/1.1\r\nHost: gateway.test\r\nAuthorization: Bearer %s\r\n\r\n", fullKey),
	}
	for _, request := range requests {
		if _, err := io.WriteString(conn, request); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("complete upload: status %d body %s error %v", resp.StatusCode, body, err)
		}
	}
}
