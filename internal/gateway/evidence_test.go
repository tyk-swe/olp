package gateway

import (
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/fixtures"
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

func TestSuccessfulEnvelopeCarriesTheRuntimeSeams(t *testing.T) {
	h := newHarness(t, Config{})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if env.Operation != operationGeneration || env.Surface != surfaceOpenAI || env.Mode != "unary" || env.ErrorClass != "" {
		t.Fatalf("envelope identity %+v", env)
	}
	if env.RuntimeGenerationID != h.rt.release.Snapshot.Generation.ID {
		t.Fatalf("envelope generation %q, want %q", env.RuntimeGenerationID, h.rt.release.Snapshot.Generation.ID)
	}
	if env.CompletedAt.Before(env.StartedAt) || env.CompletedAt.IsZero() || env.FirstByte == nil || *env.FirstByte <= 0 || *env.FirstByte > env.Duration {
		t.Fatalf("envelope timing %+v first byte %v", env, env.FirstByte)
	}
	if len(env.Attempts) != 1 {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	a := env.Attempts[0]
	if !a.UsageObserved || !a.UsageComplete || a.BillingUncertain {
		t.Fatalf("observed usage did not settle the attempt: %+v", a)
	}
	if a.Mode != "unary" || a.FirstByte == nil || *a.FirstByte <= 0 || a.RetryAfter != nil {
		t.Fatalf("attempt seams %+v first byte %v", a, a.FirstByte)
	}
}

func TestUnmeteredSuccessLeavesBillingUncertain(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", evidenceUnmetered(modelA))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if env.Outcome != "success" || len(env.Attempts) != 1 {
		t.Fatalf("envelope %+v", env)
	}
	a := env.Attempts[0]
	if a.Class != classSuccess || a.UsageObserved || a.UsageComplete || !a.BillingUncertain {
		t.Fatalf("unmetered success settled the attempt: %+v", a)
	}
}

func TestDispatchedFailureLeavesBillingUncertainAndFailsOver(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", status(http.StatusInternalServerError, `{"error":{"message":"boom"}}`))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if env.Outcome != "success" || env.ErrorClass != "" || len(env.Attempts) != 2 {
		t.Fatalf("envelope %+v", env)
	}
	failed := env.Attempts[0]
	if failed.Class != classUpstreamServer || failed.Status != http.StatusInternalServerError {
		t.Fatalf("first attempt %+v", failed)
	}
	if failed.UsageObserved || failed.UsageComplete || !failed.BillingUncertain || failed.FirstByte == nil {
		t.Fatalf("a dispatched failure was treated as certain: %+v", failed)
	}
	if served := env.Attempts[1]; served.Class != classSuccess || !served.UsageObserved || !served.UsageComplete || served.BillingUncertain {
		t.Fatalf("second attempt %+v", served)
	}
}

func TestServedButUnreadableAttemptLeavesBillingUncertain(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", evidenceTruncated)
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	a := env.Attempts[0]
	if a.Status != http.StatusOK || a.Class != classConnect || a.FirstByte == nil {
		t.Fatalf("first attempt %+v first byte %v", a, a.FirstByte)
	}
	if a.UsageObserved || a.UsageComplete || !a.BillingUncertain {
		t.Fatalf("an attempt the upstream served was treated as certain: %+v", a)
	}
}

func TestStatedRejectionsAreCertain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		handler    http.HandlerFunc
		status     int
		errorClass string
		retryAfter bool
	}{
		{"rate limit", evidenceRateLimited, http.StatusTooManyRequests, "upstream_rate_limit", true},
		{"rejected request", status(http.StatusBadRequest, `{"error":{"message":"bad","type":"invalid_request_error"}}`), http.StatusBadRequest, "upstream_rejected", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.mock.set("a", tc.handler)
			h.mock.set("b", tc.handler)
			resp, _ := h.chat(fullKey, nil)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d", resp.StatusCode)
			}
			env := h.sink.last(t)
			if env.Outcome != "failure" || env.ErrorClass != tc.errorClass || env.FirstByte != nil || env.CompletedAt.IsZero() {
				t.Fatalf("envelope %+v", env)
			}
			if len(env.Attempts) == 0 {
				t.Fatal("no attempt recorded")
			}
			for _, a := range env.Attempts {
				if a.UsageObserved || !a.UsageComplete || a.BillingUncertain {
					t.Fatalf("a stated rejection was treated as uncertain: %+v", a)
				}
				if a.FirstByte == nil {
					t.Fatalf("attempt first byte missing: %+v", a)
				}
				if got := a.RetryAfter != nil; got != tc.retryAfter {
					t.Fatalf("retry after %v, want present=%v", a.RetryAfter, tc.retryAfter)
				}
				if tc.retryAfter && *a.RetryAfter != 2*time.Second {
					t.Fatalf("retry after %v", *a.RetryAfter)
				}
			}
		})
	}
}

func TestUndispatchedFailureIsCertain(t *testing.T) {
	h := newHarness(t, Config{})
	h.upstream.Close() // nothing can reach either provider
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	env := h.sink.last(t)
	if env.Outcome != "failure" || env.ErrorClass != "upstream_unavailable" || len(env.Attempts) == 0 {
		t.Fatalf("envelope %+v", env)
	}
	for _, a := range env.Attempts {
		if a.Class != classConnect || a.FirstByte != nil {
			t.Fatalf("attempt %+v", a)
		}
		if a.UsageObserved || !a.UsageComplete || a.BillingUncertain {
			t.Fatalf("a failure that never left the gateway was treated as uncertain: %+v", a)
		}
	}
}

func TestEarlyFailureEnvelopeCarriesItsErrorClass(t *testing.T) {
	h := newHarness(t, Config{})
	resp, _ := h.chat("olp_unknown_key", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	env := h.sink.last(t)
	if env.ErrorClass != "invalid_api_key" || env.Outcome != "failure" || len(env.Attempts) != 0 || env.FirstByte != nil {
		t.Fatalf("envelope %+v", env)
	}
	if env.CompletedAt.Before(env.StartedAt) || env.CompletedAt.IsZero() {
		t.Fatalf("envelope timing %+v", env)
	}
}

func TestStreamingEnvelopeRecordsModeAndFirstByte(t *testing.T) {
	h := newHarness(t, Config{})
	data, err := fixtures.Files.ReadFile("streams/openai-chat.sse")
	if err != nil {
		t.Fatal(err)
	}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if err := testutil.Stream(w, r, data, 1, 0); err != nil {
			t.Error(err)
		}
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	if _, err = io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	env := h.sink.last(t)
	if env.Mode != "streaming" || env.Outcome != "success" || len(env.Attempts) != 1 {
		t.Fatalf("envelope %+v", env)
	}
	if env.FirstByte == nil || *env.FirstByte <= 0 || *env.FirstByte > env.Duration {
		t.Fatalf("envelope first byte %v duration %v", env.FirstByte, env.Duration)
	}
	a := env.Attempts[0]
	if a.Mode != "streaming" || a.FirstByte == nil || !a.Committed {
		t.Fatalf("attempt %+v first byte %v", a, a.FirstByte)
	}
}
