package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/upstream"
)

// Provider defaults may request usage, but only the caller opts
// into seeing it in the downstream stream.

// Usage-only chat frames are hidden unless requested by the
// client, but must still survive EOF without [DONE].

func TestResponsesClientWriteFailureBeforeTerminalRetainsPartialEvidence(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"partial-response\",\"object\":\"response\",\"status\":\"in_progress\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n")
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"team-chat","input":"hi","stream":true}`)).WithContext(t.Context())
	r.Header.Set("Authorization", "Bearer "+fullKey)
	r.Header.Set("Content-Type", "application/json")
	w := &unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), writeErr: io.ErrClosedPipe, beforeDelivery: func() {}}
	h.gateway.inference(wire.Responses)(w, r)
	env := h.sink.last(t)
	if env.Outcome != "cancelled" || len(env.Attempts) != 1 || env.Attempts[0].Class != classCancelled || h.mock.count("b") != 0 {
		t.Fatalf("partial native work became a completed provider outcome: %+v", env)
	}
	fact := env.Attempts[0]
	if !fact.UsageObserved || fact.UsageComplete || !fact.BillingUncertain || fact.Usage == nil || fact.Usage.TotalTokens != 5 {
		t.Fatalf("delivery failure changed partial native accounting evidence: %+v", fact)
	}
	stats := h.gateway.Health().ProviderHealth(time.Minute)[fact.ProviderID]
	if stats.Attempts != 1 || stats.Successes != 0 || stats.TransportErrors != 0 || stats.ServerErrors != 0 {
		t.Fatalf("partial native work changed provider health: %+v", stats)
	}
}

func TestResponseNativeWriterFailureKeepsRetainedRetryFence(t *testing.T) {
	for _, retained := range []bool{true, false} {
		for _, phase := range []string{"completed", "failed", "partial"} {
			t.Run(fmt.Sprintf("retained=%t/%s", retained, phase), func(t *testing.T) {
				h := newHarness(t, Config{})
				kind, status := phase, phase
				if phase == "partial" {
					kind, status = "created", "in_progress"
				}
				nativeFailure := ""
				if phase == "failed" {
					nativeFailure = `,"error":{"type":"server_error","code":"server_error","message":"native generation failed"}`
				}
				encodedWire := fmt.Sprintf("data: {\"type\":\"response.%s\",\"response\":{\"id\":\"native-response\",\"object\":\"response\",\"status\":%q,\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}%s}}\n\n", kind, status, nativeFailure)
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, encodedWire)
				})
				parsed, err := openai.Parse(wire.Responses, fmt.Appendf(nil, `{"model":"team-chat","input":"hi","stream":true,"store":%t}`, retained))
				if err != nil {
					t.Fatal(err)
				}
				release := h.rt.Release()
				route := release.Snapshot.Routes[routeSlug]
				target := route.Targets[0]
				provider := release.Snapshot.Providers[target.ProviderID]
				x := &call{
					request: request{release: release, startedAt: time.Now()},
					family:  wire.Responses,

					providerState: true, routeLeg: routeLeg{route: &route}, generation: generationCall{parsed: parsed,

						emit: func([]byte) error { return errClientWrite }},
				}
				attempt := routing.Attempt{ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, UpstreamModel: target.ProviderModel, Timeout: 2 * time.Second}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				fact, _, failure := h.gateway.attempt(ctx, x, attempt, &provider, provider.Slots[0], AttemptFact{Ordinal: 1, StartedAt: time.Now()})
				if failure == nil || failure.class != classCancelled || !failure.dispatched || !failure.accepted || h.mock.count("a") != 1 {
					t.Fatalf("native client failure changed actual delivery outcome: failure=%+v fact=%+v", failure, fact)
				}
				wantAcceptance, wantClass := upstream.Terminal, classSuccess
				if phase == "failed" {
					wantClass = classUpstreamServer
				} else if phase == "partial" {
					wantAcceptance, wantClass = upstream.Accepted, classCancelled
				}
				if failure.acceptance != wantAcceptance || fact.Class != wantClass {
					t.Fatalf("native outcome changed retry evidence: failure=%+v fact=%+v", failure, fact)
				}
				if failure.noRetry != retained {
					t.Fatalf("accepted retained invocation lost its retry fence: retained=%t phase=%s noRetry=%t", retained, phase, failure.noRetry)
				}
			})
		}
	}
}
