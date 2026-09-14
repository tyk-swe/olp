package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestTerminalStreamReleasesAdmissionAndRecordsSuccessWithoutEOF(t *testing.T) {
	for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses} {
		t.Run(string(family), func(t *testing.T) {
			h := newHarness(t, Config{})
			closed := make(chan struct{})
			terminal := "[DONE]"
			body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
			wire := "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"
			if family == openai.FamilyResponses {
				terminal = "response.completed"
				body = `{"model":"team-chat","input":"hi","stream":true}`
				wire = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"model-a\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n"
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, wire)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(closed)
			})
			r := httptest.NewRequest(http.MethodPost, "/v1"+endpointPath(family), strings.NewReader(body)).WithContext(t.Context())
			r.Header.Set("Authorization", "Bearer "+fullKey)
			r.Header.Set("Content-Type", "application/json")
			w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), beforeDelivery: func() {}}
			h.gateway.inference(family)(w, r)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), terminal) || strings.Contains(w.Body.String(), `"error"`) {
				t.Fatalf("terminal output: %d %s", w.Code, w.Body.String())
			}
			env := h.sink.last(t)
			if len(h.sink.envs) != 1 || env.Outcome != "success" || !env.Committed || env.Usage == nil || env.Usage.TotalTokens != 5 || len(env.Attempts) != 1 || env.Attempts[0].Class != classSuccess {
				t.Fatalf("terminal accounting: %+v", env)
			}
			if len(h.gateway.admission) != 0 {
				t.Fatal("terminal stream retained admission")
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("terminal stream retained the upstream connection")
			}
		})
	}
}
