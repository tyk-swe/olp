package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestStreamsUsePerEventLimitNotUnaryResponseLimit(t *testing.T) {
	for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses} {
		for _, oversizedEvent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/oversized-event=%t", family, oversizedEvent), func(t *testing.T) {
				cfg := Config{MaxInFlight: 1, MaxBodyBytes: 4096, MaxResponseBytes: 1024, MaxEventBytes: 512}
				h := newHarness(t, cfg)
				body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
				chunk := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"piece\"},\"finish_reason\":null}]}\n\n"
				terminal := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				marker := "data: [DONE]"
				if family == openai.FamilyResponses {
					body = `{"model":"team-chat","input":"hi","stream":true}`
					chunk = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"piece\"}\n\n"
					terminal = "data: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n"
					marker = "event: response.completed"
				}
				const chunks = 24
				wire := strings.Repeat(chunk, chunks)
				if int64(len(wire)) <= cfg.MaxResponseBytes {
					t.Fatal("fixture must exceed the unary response limit before its terminal event")
				}
				if oversizedEvent {
					wire += "data: " + strings.Repeat("x", int(cfg.MaxEventBytes)) + "\n\n"
				}
				wire += terminal
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, wire)
				})
				r := httptest.NewRequest(http.MethodPost, "/v1"+endpointPath(family), strings.NewReader(body)).WithContext(t.Context())
				r.Header.Set("Authorization", "Bearer "+fullKey)
				r.Header.Set("Content-Type", "application/json")
				w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), beforeDelivery: func() {}}
				h.gateway.inference(family)(w, r)
				if w.Code != http.StatusOK || strings.Count(w.Body.String(), "piece") != chunks {
					t.Fatalf("valid events were lost: %d %s", w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), marker) == oversizedEvent || strings.Contains(w.Body.String(), `"code":"resource_exhausted"`) != oversizedEvent {
					t.Fatalf("incorrect terminal frame: %s", w.Body.String())
				}
				env := h.sink.last(t)
				wantOutcome, wantClass := "success", classSuccess
				if oversizedEvent {
					// A per-event byte ceiling is a bounded local resource,
					// not provider ill health.
					wantOutcome, wantClass = "failure", classResourceExhausted
				}
				if env.Outcome != wantOutcome || !env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Class != wantClass || h.mock.count("b") != 0 {
					t.Fatalf("incorrect terminal accounting: %+v", env)
				}
			})
		}
	}
}
