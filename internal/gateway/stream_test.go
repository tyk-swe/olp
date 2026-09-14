package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPrematureDoneReturnsAnErrorWithoutASuccessMarker(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name, wantStatus := "before first frame", http.StatusBadGateway
		if partial {
			name, wantStatus = "after partial output", http.StatusOK
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if partial {
					io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","model":"model-a","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
				}
				io.WriteString(w, "data: [DONE]\n\n")
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			if resp.StatusCode != wantStatus || strings.Contains(body, "[DONE]") || !strings.Contains(body, `"code":"provider_protocol_error"`) {
				t.Fatalf("status %d body %s", resp.StatusCode, body)
			}
			if h.mock.count("b") != 0 {
				t.Fatal("a protocol error must not fail over")
			}
		})
	}
}

func TestUnfinishedSecondChoiceRecordsFailureWithoutASuccessMarker(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","model":"model-a","choices":[{"index":0,"delta":{"content":"complete"},"finish_reason":"stop"},{"index":1,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\ndata: [DONE]\n\n")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true,"n":2}`), nil)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "[DONE]") || !strings.Contains(body, `"content":"partial"`) || !strings.Contains(body, `"code":"provider_protocol_error"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if env.Outcome != "failure" || !env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Class != classProtocol || !env.Attempts[0].Committed {
		t.Fatalf("envelope %+v", env)
	}
	if h.mock.count("b") != 0 {
		t.Fatal("a committed stream must not fail over")
	}
}

func TestMalformedSSEDoesNotFailOver(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: \xff\n\n")
			})
			body := `{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`
			if path == "/v1/responses" {
				body = `{"model":"team-chat","input":"hi","stream":true}`
			}
			resp := h.do(t.Context(), http.MethodPost, path, fullKey, []byte(body), nil)
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(raw), `"code":"provider_protocol_error"`) {
				t.Fatalf("status %d body %s", resp.StatusCode, raw)
			}
			env := h.sink.last(t)
			if env.Outcome != "failure" || env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Class != classProtocol || h.mock.count("b") != 0 {
				t.Fatalf("malformed SSE was not terminal: %+v", env)
			}
		})
	}
}
