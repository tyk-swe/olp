package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestAmbiguousStreamCredentialFailuresRequestRefreshWithoutFailover(t *testing.T) {
	for _, carried := range []bool{false, true} {
		for _, committed := range []bool{false, true} {
			for _, code := range []string{"invalid_api_key", "rate_limit_exceeded"} {
				t.Run(fmt.Sprintf("carried_%t/committed_%t/%s", carried, committed, code), func(t *testing.T) {
					var h *harness
					if carried {
						h = newCarryingHarness(t, carrierFunc(func(ctx context.Context, _ abi.Provider, req *http.Request) (*http.Response, error) {
							return forward(ctx, req)
						}))
					} else {
						h = strictHarness(t, nil)
					}
					h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						if committed {
							fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n", modelA)
						}
						fmt.Fprintf(w, "data: {\"error\":{\"message\":\"request refused\",\"type\":\"invalid_request_error\",\"code\":%q}}\n\n", code)
					})
					resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					wantStatus := http.StatusBadGateway
					if committed {
						wantStatus = http.StatusOK
					}
					if err != nil || resp.StatusCode != wantStatus || !strings.Contains(string(body), "ambiguous_upstream_result") {
						t.Fatalf("status %d body %s: %v", resp.StatusCode, body, err)
					}
					attempts := h.sink.last(t).Attempts
					if len(attempts) != 1 || attempts[0].Class != classAmbiguous || attempts[0].Committed != committed || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
						t.Fatalf("attempts %+v; upstream calls %d/%d", attempts, h.mock.count("a"), h.mock.count("b"))
					}
					var want []string
					if code == "invalid_api_key" {
						want = []string{h.credA}
					}
					if refused := h.rt.refusals(); !slices.Equal(refused, want) {
						t.Fatalf("refused credential versions %v, want %v", refused, want)
					}
				})
			}
		}
	}
}
