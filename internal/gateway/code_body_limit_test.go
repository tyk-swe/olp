package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/codexwire"
)

func TestCodeWebSocketEnforcesConfiguredRequestLimit(t *testing.T) {
	for _, test := range []struct {
		name     string
		limit    int64
		size     int
		accepted bool
	}{
		{"at configured limit", 64 << 10, 64 << 10, true},
		{"above configured limit", 64 << 10, 128 << 10, false},
		{"default limit", 0, 128 << 10, true},
		{"above protocol limit", codexwire.MaxBody + 1024, codexwire.MaxBody + 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			h.gateway.cfg.MaxBodyBytes = test.limit
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			seen := make(chan []byte, 1)
			response := `{"type":"response.completed","response":{"output":"` + strings.Repeat("x", 128<<10) + `","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				conn.SetReadLimit(codexwire.MaxBody)
				_, body, err := conn.Read(ctx)
				seen <- body
				if err == nil {
					_ = conn.Write(ctx, websocket.MessageText, []byte(response))
					_, _, _ = conn.Read(ctx)
				}
			})
			conn, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"body-limit"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			conn.SetReadLimit(codexwire.MaxBody)
			prefix, suffix := `{"type":"response.create","model":"native-model","input":"`, `"}`
			body := prefix + strings.Repeat("x", test.size-len(prefix)-len(suffix)) + suffix
			_ = conn.Write(ctx, websocket.MessageText, []byte(body))
			_, got, err := conn.Read(ctx)
			if test.accepted {
				if err != nil || string(got) != response {
					t.Fatalf("allowed request or larger upstream response refused: %v", err)
				}
				if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
					t.Fatal("allowed generation lost usage")
				}
			} else if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
				t.Fatalf("oversized request was not refused: %v", err)
			}
			select {
			case got := <-seen:
				if test.accepted && string(got) != body || !test.accepted && len(got) != 0 {
					t.Fatal("request bytes changed or oversized request dispatched")
				}
			case <-ctx.Done():
				t.Fatal("upstream connection did not finish reading")
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if !test.accepted && (len(ledger.inputs) != 0 || len(ledger.marks) != 0) {
				t.Fatal("oversized request admitted")
			}
		})
	}
}
