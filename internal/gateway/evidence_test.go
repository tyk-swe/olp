package gateway

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/usage"
)

// evidenceUnmetered answers with a well-formed completion that reports no
// usage at all: the upstream served and billed work nothing can meter.
func evidenceUnmetered(model string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, model, answerText)
	}
}

// evidenceTruncated answers 200 and then drops the connection part way
// through the body: the upstream served the request but nothing the gateway
// can read says what it consumed.
func evidenceTruncated(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", "512")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion",`)
	if err := http.NewResponseController(w).Flush(); err != nil {
		panic(err)
	}
	panic(http.ErrAbortHandler)
}

// evidenceRateLimited answers with a stated rejection that costs nothing.
func evidenceRateLimited(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "2")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
}

func testStrictUnreadableUnaryResult(t *testing.T, body string) {
	t.Helper()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		for id, provider := range snapshot.Providers {
			provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "embeddings", Surface: "openai", Mode: "unary"}}
			snapshot.Providers[id] = provider
		}
		route := snapshot.Routes[routeSlug]
		route.Operations = []string{"embeddings"}
		snapshot.Routes[routeSlug] = route
	})
	h.mock.set("a", status(http.StatusOK, body))
	resp := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hi"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s dispatches=%d/%d", resp.StatusCode, body, h.mock.count("a"), h.mock.count("b"))
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	a := env.Attempts[0]
	if a.Interaction == nil || a.Interaction.UpstreamState != usage.UpstreamTerminal || a.Interaction.ClientState != usage.ClientUnobserved {
		t.Errorf("complete upstream response lost its interaction evidence: %+v", a.Interaction)
	}
	if a.Status != http.StatusOK || a.UsageObserved || a.UsageComplete || !a.BillingUncertain {
		t.Errorf("an accepted but unreadable result settled billing: %+v", a)
	}
	x := &call{estimate: 100, facts: env.Attempts}
	if got := *x.settledTokens(); got != x.estimate {
		t.Errorf("unmetered accepted work settled %d tokens, want estimate %d", got, x.estimate)
	}
}

// nothing can reach either provider
