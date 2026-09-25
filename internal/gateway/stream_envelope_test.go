package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The provider-authored SSE envelope (id/retry/event) and independently
// authored native members must reach the client through the gateway, while
// usage presentation stays field-scoped and accounting stays independent.
func TestChatStreamEnvelopeAndExtraFieldsSurviveGateway(t *testing.T) {
	for _, options := range []string{"", `{"include_usage":true}`} {
		t.Run("options="+options, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "id: upstream-9\nretry: 0\n")
				io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"`+modelA+`","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"prompt_filter_results":[{"content_filter_results":{"hate":{"filtered":false}}}]}`+"\n\n")
				io.WriteString(w, "event: annotations\n")
				io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"`+modelA+`","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5},"prompt_filter_results":[{"content_filter_results":{"hate":{"filtered":false}}}]}`+"\n\n")
				io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"`+modelA+`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
				io.WriteString(w, "data: [DONE]\n\n")
			})
			body := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}],"stream":true`
			if options != "" {
				body += `,"stream_options":` + options
			}
			resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body+"}"), nil)
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			wantUsage := options != ""
			out := string(raw)
			if resp.StatusCode != http.StatusOK || !strings.HasSuffix(out, "data: [DONE]\n\n") {
				t.Fatalf("stream did not complete: %d %s", resp.StatusCode, out)
			}
			if !strings.Contains(out, "id: upstream-9\n") || !strings.Contains(out, "retry: 0\n") || !strings.Contains(out, "event: annotations\n") {
				t.Fatalf("SSE envelope was stripped by the gateway: %s", out)
			}
			if strings.Count(out, "prompt_filter_results") != 2 {
				t.Fatalf("independently authored native member was lost: %s", out)
			}
			if strings.Contains(out, `"usage":`) != wantUsage {
				t.Fatalf("client usage option was ignored: %s", out)
			}
			if strings.Contains(out, modelA) || !strings.Contains(out, `"model":"`+routeSlug+`"`) {
				t.Fatalf("route model overlay lost: %s", out)
			}
			// Metering and settlement stay independent of presentation: one
			// attempt, one provider call, full observed usage.
			env := h.sink.last(t)
			if env.Outcome != "success" || env.Usage == nil || env.Usage.TotalTokens != 5 || len(env.Attempts) != 1 || env.Attempts[0].Usage == nil || env.Attempts[0].Usage.TotalTokens != 5 {
				t.Fatalf("internal usage accounting was lost: %+v", env)
			}
			if h.mock.count("a") != 1 {
				t.Fatalf("residual events replayed inference: %d provider calls", h.mock.count("a"))
			}
		})
	}
}
