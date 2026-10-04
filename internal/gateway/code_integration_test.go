package gateway

import (
	"bytes"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"
)

func TestCodeZstandardBytesAndQuerySurviveObservation(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	raw := encoder.EncodeAll([]byte(`{"model":"native-model","input":[]}`), nil)
	h.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, raw) || r.Header.Get("Content-Encoding") != "zstd" || r.URL.RawQuery != "opaque=%2F%2b&repeat=a&repeat=b" {
			t.Error("encoded body, encoding or query changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "zstd")
		w.Write(encoder.EncodeAll([]byte(`{"status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`), nil))
	})
	request, _ := http.NewRequest("POST", server.URL+"/code/coding/responses?opaque=%2F%2b&repeat=a&repeat=b", bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+fullKey)
	request.Header.Set("Thread-Id", "zstd-thread")
	request.Header.Set("Content-Encoding", "zstd")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || response.Header.Get("Content-Encoding") != "zstd" || len(body) == 0 {
		t.Fatalf("encoded response unavailable: %d", response.StatusCode)
	}
	if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 6 {
		t.Fatal("encoded response usage not observed")
	}
}

func TestCodeWebSocketHandshakePreservesSuccessAndLargeRefusal(t *testing.T) {
	for _, status := range []int{101, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			body := strings.Repeat("opaque-handshake-refusal", 300)
			h.upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header()["X-Provider-Handshake"] = []string{"first", "second"}
				if status != 101 {
					w.WriteHeader(status)
					io.WriteString(w, body)
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_, _, _ = conn.Read(r.Context())
			})
			request, _ := http.NewRequest("GET", server.URL+"/code/coding/responses", nil)
			request.Header = http.Header{
				"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"handshake"},
				"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
				"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="},
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != status || !reflect.DeepEqual(response.Header.Values("X-Provider-Handshake"), []string{"first", "second"}) {
				t.Fatalf("handshake status or headers changed: %d", response.StatusCode)
			}
			if status != 101 {
				got, _ := io.ReadAll(response.Body)
				if string(got) != body {
					t.Fatal("handshake refusal body truncated")
				}
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.marks) != 0 || len(ledger.inputs) != 0 {
				t.Fatal("handshake authorized inference")
			}
		})
	}
}
