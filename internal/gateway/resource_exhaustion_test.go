package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/interaction"
)

// A bounded local byte budget is a resource this gateway ran out of, not
// provider ill health: the attempt is terminal, the class is distinct, and
// the provider's circuit and transport statistics stay clean.
func TestStreamEventByteCeilingIsLocalResourceExhaustion(t *testing.T) {
	h := newHarness(t, Config{MaxInFlight: 1, MaxBodyBytes: 4096, MaxResponseBytes: 1 << 20, MaxEventBytes: 512})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\""+strings.Repeat("x", 600)+"\"}}]}\n\n")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(raw), `"code":"resource_exhausted"`) || !strings.Contains(string(raw), "event_bytes") {
		t.Fatalf("status %d body %s", resp.StatusCode, raw)
	}
	env := h.sink.last(t)
	if env.Outcome != "failure" || len(env.Attempts) != 1 || env.Attempts[0].Class != classResourceExhausted || h.mock.count("b") != 0 {
		t.Fatalf("local exhaustion must be terminal without failover: %+v", env)
	}
	for id, stats := range h.gateway.health.ProviderHealth(time.Hour) {
		if stats.TransportErrors != 0 || stats.ServerErrors != 0 {
			t.Fatalf("provider %s penalized for local exhaustion: %+v", id, stats)
		}
	}
	if h.gateway.health.open(env.Attempts[0].ProviderID) {
		t.Fatal("local exhaustion opened the provider circuit")
	}
}

// The unary response ceiling is the same bounded local resource.
func TestUnaryResponseByteCeilingLeavesProviderHealthUntouched(t *testing.T) {
	h := newHarness(t, Config{MaxInFlight: 1, MaxBodyBytes: 4096, MaxResponseBytes: 512, MaxEventBytes: 4096})
	h.mock.set("a", completion(modelA, strings.Repeat("y", 1000)))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "resource_exhausted" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 || env.Attempts[0].Class != classResourceExhausted || h.mock.count("b") != 0 {
		t.Fatalf("response exhaustion must be terminal without failover: %+v", env)
	}
	for id, stats := range h.gateway.health.ProviderHealth(time.Hour) {
		if stats.TransportErrors != 0 || stats.ServerErrors != 0 {
			t.Fatalf("provider %s penalized for local exhaustion: %+v", id, stats)
		}
	}
}

// The one-hour stream lifetime cap is a proxy-local time bound: the provider
// delivered the whole time, so it must not read as provider ill health.
func TestStreamLifetimeCapClassifiesAsTimeExhaustion(t *testing.T) {
	st := &attemptState{parent: context.Background()}
	st.reason.Store(3)
	if f := st.classify(io.EOF, true); f.class != classResourceExhausted {
		t.Fatalf("stream cap classified %q", f.class)
	}
	f := &attemptFailure{class: classResourceExhausted, exhausted: &interaction.Exhaustion{
		Resource: "stream_lifetime_seconds", Category: interaction.LimitTime, Limit: int(maxStreamDuration / time.Second),
	}}
	e := f.toError()
	if e.Code != "resource_exhausted" || !strings.Contains(e.Message, "stream_lifetime_seconds") || !strings.Contains(e.Message, "time") {
		t.Fatalf("time exhaustion not reported by resource: %+v", e)
	}
	if failoverAllowed(f.class, f.committed) {
		t.Fatal("lifetime exhaustion must not fail over")
	}
}

// Projection budget failures classify by their own resource, not as protocol
// defects, whether or not earlier frames committed.
func TestProjectionExhaustionClassifiesByResource(t *testing.T) {
	f := &attemptFailure{
		class:     classResourceExhausted,
		exhausted: &interaction.Exhaustion{Resource: "retained_dependency_bytes", Category: interaction.LimitBytes, Limit: 4 << 20},
	}
	e := f.toError()
	if e.Code != "resource_exhausted" || !strings.Contains(e.Message, "retained_dependency_bytes") {
		t.Fatalf("dependency exhaustion not reported by resource: %+v", e)
	}
	st := &attemptState{parent: context.Background()}
	if f := st.classify(&interaction.Exhaustion{Resource: "admitted_event_work_bytes", Category: interaction.LimitEventWork, Limit: 8}, true); f.class != classResourceExhausted {
		t.Fatalf("projection exhaustion classified %q", f.class)
	}
}
