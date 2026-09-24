package gateway

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type unaryResponseWriter struct {
	*httptest.ResponseRecorder
	writeErr, flushErr error
	deadline           time.Time
	beforeDelivery     func()
}

func (w *unaryResponseWriter) SetReadDeadline(time.Time) error { return nil }
func (w *unaryResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}
func (w *unaryResponseWriter) Write(p []byte) (int, error) {
	w.beforeDelivery()
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.ResponseRecorder.Write(p)
}
func (w *unaryResponseWriter) FlushError() error {
	w.beforeDelivery()
	return w.flushErr
}

func TestInferenceErrorDeliveryHasWriteDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, key, model string
		status           int
	}{
		{"authentication", "invalid-key", "team-chat", http.StatusUnauthorized},
		{"unknown model", fullKey, "missing", http.StatusNotFound},
		{"upstream rejection", fullKey, "team-chat", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", status(http.StatusBadRequest, `{"error":{"message":"rejected"}}`))
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, tc.model)
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			r.Header.Set("Authorization", "Bearer "+tc.key)
			r.Header.Set("Content-Type", "application/json")
			w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder()}
			wrote := false
			w.beforeDelivery = func() {
				wrote = true
				if remaining := time.Until(w.deadline); remaining <= 0 || remaining > responseWriteTimeout {
					t.Fatalf("error delivery has no bounded write deadline: %v", w.deadline)
				}
			}
			h.gateway.inference(openai.FamilyChat)(w, r)
			if !wrote || w.Code != tc.status || h.gateway.admission.Admitted() != 0 {
				t.Fatalf("error delivery: wrote=%t status=%d admission=%d", wrote, w.Code, h.gateway.admission.Admitted())
			}
		})
	}
}

func TestUnaryDeliveryDeterminesTerminalOutcome(t *testing.T) {
	for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses} {
		for _, tc := range []struct {
			name               string
			writeErr, flushErr error
		}{
			{"success", nil, nil},
			{"write failure", io.ErrClosedPipe, nil},
			{"flush failure", nil, io.ErrClosedPipe},
		} {
			t.Run(string(family)+"/"+tc.name, func(t *testing.T) {
				h := newHarness(t, Config{})
				body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}]}`
				if family == openai.FamilyResponses {
					body = `{"model":"team-chat","input":"hi"}`
					h.mock.set("a", status(http.StatusOK, `{"id":"resp_1","object":"response","model":"model-a","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`))
				}
				r := httptest.NewRequest(http.MethodPost, "/v1"+endpointPath(family), strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+fullKey)
				r.Header.Set("Content-Type", "application/json")
				w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: tc.writeErr, flushErr: tc.flushErr}
				w.beforeDelivery = func() {
					if remaining := time.Until(w.deadline); remaining <= 0 || remaining > responseWriteTimeout {
						t.Fatalf("delivery has no bounded write deadline: %v", w.deadline)
					}
					if len(h.sink.envs) != 0 {
						t.Fatal("terminal outcome recorded before delivery finished")
					}
				}
				h.gateway.inference(family)(w, r)
				if len(h.sink.envs) != 1 || h.gateway.admission.Admitted() != 0 {
					t.Fatalf("terminal envelopes=%d admission=%d", len(h.sink.envs), h.gateway.admission.Admitted())
				}
				env := h.sink.last(t)
				wantOutcome, wantStatus := "success", http.StatusOK
				if tc.writeErr != nil || tc.flushErr != nil {
					wantOutcome, wantStatus = "cancelled", 0
				}
				if env.Outcome != wantOutcome || env.Status != wantStatus || !env.Committed {
					t.Fatalf("envelope %+v", env)
				}
				if len(env.Attempts) != 1 || env.Attempts[0].Class != classSuccess || env.Attempts[0].Usage == nil || env.Attempts[0].Usage.TotalTokens != 5 {
					t.Fatalf("delivery changed successful upstream accounting: %+v", env.Attempts)
				}
				if h.mock.count("a") != 1 || h.mock.count("b") != 0 {
					t.Fatal("client delivery must not trigger another upstream attempt")
				}
			})
		}
	}
}

func TestMalformedNativeRequestKeepsDialectTelemetry(t *testing.T) {
	for _, tc := range []struct{ dialect, operation string }{
		{"cohere-embed-v2", "embeddings"},
		{"cohere-rerank-v2", "rerank"},
		{"tei-tokenize", "token_count"},
		{"openai-moderation", "moderation"},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			h := strictHarness(t, nil)
			resp := h.do(t.Context(), http.MethodPost, "/native/"+tc.dialect+"/models/"+routeSlug, fullKey, []byte(`{"model":`), nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d body %s", resp.StatusCode, raw)
			}
			env := h.sink.last(t)
			if env.Operation != tc.operation || env.Surface != "native" || env.Mode != "unary" || env.Outcome != "failure" {
				t.Fatalf("malformed native request lost its dialect identity: %+v", env)
			}
		})
	}
}

func TestUnreadUnaryResponseReleasesAdmission(t *testing.T) {
	t.Parallel()
	h := newHarness(t, Config{MaxInFlight: 1, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 16 << 20, MaxEventBytes: 4096})
	h.mock.set("a", completion(modelA, strings.Repeat("x", 8<<20)))
	finished := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.server.Config.Handler.ServeHTTP(w, r)
		finished <- struct{}{}
	}))
	defer server.Close()
	conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}]}`
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: gateway.test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", fullKey, len(body), body); err != nil {
		t.Fatal(err)
	}
	// Keep the connection open without reading any of the large response.
	select {
	case <-finished:
	case <-time.After(responseWriteTimeout + 8*time.Second):
		t.Fatal("an unread unary response retained admission past the write deadline")
	}
	env := h.sink.last(t)
	if env.Outcome != "cancelled" || env.Status != 0 || !env.Committed || h.gateway.admission.Admitted() != 0 {
		t.Fatalf("unread response: envelope=%+v admission=%d", env, h.gateway.admission.Admitted())
	}
	if h.mock.count("b") != 0 {
		t.Fatal("a write timeout triggered failover")
	}
	h.mock.set("a", completion(modelA, answerText))
	if resp, body := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("admission did not recover: %d %v", resp.StatusCode, body)
	}
}
