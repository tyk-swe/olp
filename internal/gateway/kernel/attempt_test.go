package kernel

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
)

type attemptSeen struct {
	class     string
	status    int
	committed bool
}

const stalled = "<stalled>"

func providerStream(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			if frame == stalled {
				<-r.Context().Done()
				return
			}
			io.WriteString(w, "data: "+frame+"\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

func providerReply(status int, headers map[string]string, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

var replyErrorCode = regexp.MustCompile(`"error":\{"code":"([a-z_]+)"`)

func TestEachKindOfProviderFailureIsClassifiedAndFallsBackOnlyWhileTheReplyIsUncommitted(t *testing.T) {
	const (
		delta     = `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`
		inBand    = `{"error":{"message":"boom","type":"server_error"}}`
		oversized = 5000

		routeDeadline = 5 * time.Second
	)
	served := attemptSeen{"success", http.StatusOK, true}
	for name, tc := range map[string]struct {
		provider http.HandlerFunc
		streamed bool
		timeout  int64
		status   int
		code     string
		attempts []attemptSeen
	}{
		"rate limited": {
			provider: providerReply(http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, `{"error":{"message":"slow down","type":"rate_limit_error"}}`),
			status:   http.StatusOK, attempts: []attemptSeen{{"rate_limit", http.StatusTooManyRequests, false}, served},
		},
		"credential refused": {
			provider: providerReply(http.StatusUnauthorized, nil, `{"error":{"message":"bad key","type":"invalid_request_error"}}`),
			status:   http.StatusOK, attempts: []attemptSeen{{"credential", http.StatusUnauthorized, false}, served},
		},
		"silent provider": {
			provider: providerStream(stalled), timeout: 80,
			status: http.StatusOK, attempts: []attemptSeen{{"timeout", 0, false}, served},
		},
		"stream opening with a provider error": {
			provider: providerStream(inBand), streamed: true,
			status: http.StatusOK, attempts: []attemptSeen{{"upstream_server", http.StatusOK, false}, served},
		},
		"unreadable reply": {
			provider: providerReply(http.StatusOK, nil, `not json`),
			status:   http.StatusBadGateway, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, false}},
		},
		"whole reply to a streamed request": {
			provider: providerReply(http.StatusOK, nil, `{}`), streamed: true,
			status: http.StatusBadGateway, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, false}},
		},
		"stream frame over the event limit": {
			provider: providerStream(`{"choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", oversized) + `"}}]}`), streamed: true,
			status: http.StatusBadGateway, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, false}},
		},
		"stream stalling after its first frame": {
			provider: providerStream(delta, stalled), streamed: true, timeout: 80,
			status: http.StatusOK, code: "gateway_timeout", attempts: []attemptSeen{{"timeout", http.StatusOK, true}},
		},
		"stream ending without a final frame": {
			provider: providerStream(delta), streamed: true,
			status: http.StatusOK, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, true}},
		},
		"provider error after the first frame": {
			provider: providerStream(delta, inBand), streamed: true,
			status: http.StatusOK, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{})
			if tc.timeout > 0 {
				h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
					route := s.Routes[routeSlug]
					for i := range route.Targets {
						route.Targets[i].Timeout = tc.timeout
					}
					s.Routes[routeSlug] = route
				})
			}
			h.mock.set("a", tc.provider)
			body := entryChat
			if tc.streamed {
				h.mock.set("b", streamedChat(answerText))
				body = strings.TrimSuffix(entryChat, "}") + `,"stream":true}`
			}
			started := time.Now()
			resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body), nil)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if elapsed := time.Since(started); tc.timeout > 0 && elapsed > routeDeadline/2 {
				t.Fatalf("a provider silent past its %dms target timeout held the caller for %v", tc.timeout, elapsed)
			}

			code := ""
			if found := replyErrorCode.FindSubmatch(raw); found != nil {
				code = string(found[1])
			}
			if resp.StatusCode != tc.status || code != tc.code {
				t.Fatalf("reply = %d %q, want %d %q: %.300s", resp.StatusCode, code, tc.status, tc.code, raw)
			}
			if strings.Contains(string(raw), strings.Repeat("a", oversized)) {
				t.Fatal("the caller received a frame over the event limit")
			}
			if tc.code == "" && !strings.Contains(string(raw), answerText) {
				t.Fatalf("the caller did not get the second provider's answer: %.300s", raw)
			}
			if last := tc.attempts[len(tc.attempts)-1]; tc.code != "" && strings.Contains(string(raw), "partial") != last.committed {
				t.Fatalf("partial output reached the caller = %t, want %t", !last.committed, last.committed)
			}

			env := h.sink.last(t)
			seen := make([]attemptSeen, len(env.Attempts))
			for i, attempt := range env.Attempts {
				seen[i] = attemptSeen{attempt.Class, attempt.Status, attempt.Committed}
			}
			if len(seen) != len(tc.attempts) {
				t.Fatalf("attempts = %+v, want %+v", seen, tc.attempts)
			}
			for i, want := range tc.attempts {
				if seen[i] != want {
					t.Fatalf("attempt %d = %+v, want %+v", i, seen[i], want)
				}
			}
			first := env.Attempts[0]
			if name == "rate limited" && (first.RetryAfter == nil || *first.RetryAfter != 7*time.Second) {
				t.Fatalf("retry after = %v, want the provider's 7s", first.RetryAfter)
			}
			if name == "silent provider" && first.FirstByte != nil {
				t.Fatal("a provider that never answered has a first byte time")
			}
			if succeeded := env.Outcome == "success"; succeeded != (tc.code == "") {
				t.Fatalf("outcome = %s for reply code %q", env.Outcome, tc.code)
			}
		})
	}
}
