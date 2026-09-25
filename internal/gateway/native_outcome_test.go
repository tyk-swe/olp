package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// A native Responses incomplete terminal is a provider outcome, not a protocol
// fault: the stream keeps its partial content, empty output, incomplete
// details and usage, and no synthetic proxy error follows the terminal event.
func TestNativeIncompleteStreamIsAValidTerminal(t *testing.T) {
	wire := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"model-a\",\"output\":[]}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
		"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"status\":\"incomplete\",\"model\":\"model-a\",\"output\":[],\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10}}}\n\n"
	for _, strict := range []bool{false, true} {
		t.Run(map[bool]string{true: "strict", false: "legacy"}[strict], func(t *testing.T) {
			var h *harness
			body := `{"model":"team-chat","input":"hi","stream":true}`
			if strict {
				h = strictHarness(t, func(snapshot *runtime.Snapshot) {
					for id, provider := range snapshot.Providers {
						provider.ProfileID = "compatible-responses"
						snapshot.Providers[id] = provider
					}
				})
				// The test key holds no provider-state policy; an explicit
				// store:false keeps this exchange outside retained state.
				body = `{"model":"team-chat","input":"hi","stream":true,"store":false}`
			} else {
				h = newHarness(t, Config{})
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, wire)
			})
			resp := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(body), nil)
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "event: response.incomplete") ||
				!strings.Contains(string(raw), `"status":"incomplete"`) ||
				!strings.Contains(string(raw), `"incomplete_details":{"reason":"max_output_tokens"}`) ||
				!strings.Contains(string(raw), `"delta":"partial"`) ||
				strings.Contains(string(raw), "event: error") {
				t.Fatalf("native incomplete terminal was not preserved faithfully: %d %s", resp.StatusCode, raw)
			}
			env := h.sink.last(t)
			if env.Outcome != "success" || !env.Committed || len(env.Attempts) != 1 || h.mock.count("b") != 0 {
				t.Fatalf("native incomplete outcome changed: %+v", env)
			}
			fact := env.Attempts[0]
			if fact.Class != classSuccess || fact.NativeStatus != "incomplete" || fact.FaultOrigin != "" || fact.FaultScope != "" {
				t.Fatalf("a valid native terminal carried fault attribution: %+v", fact)
			}
			if env.Usage == nil || env.Usage.TotalTokens != 10 || fact.Usage == nil || fact.Usage.TotalTokens != 10 {
				t.Fatalf("native incomplete usage was lost: %+v", env)
			}
			if strict && (fact.Interaction == nil || fact.Interaction.UpstreamState != usage.UpstreamTerminal || fact.Interaction.ClientState != usage.ClientTerminal) {
				t.Fatalf("strict incomplete interaction evidence changed: %+v", fact.Interaction)
			}
			// A valid native outcome is success evidence for the provider: the
			// shared circuit stays untouched.
			stats := h.gateway.Health().ProviderHealth(time.Minute)
			if s := stats[fact.ProviderID]; s.Attempts != 1 || s.Successes != 1 || s.TransportErrors != 0 || s.ServerErrors != 0 {
				t.Fatalf("a valid native outcome poisoned provider health: %+v", s)
			}
		})
	}
}

// EOF before the native terminal remains a provider transport fault; nothing
// about the relaxed terminal contract forgives a truncated stream.
func TestResponsesStreamEOFBeforeTerminalStaysATransportFault(t *testing.T) {
	h := newHarness(t, Config{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"model-a\",\"output\":[]}}\n\n")
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"team-chat","input":"hi","stream":true}`), nil)
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "event: error") || !strings.Contains(string(raw), "provider_protocol_error") {
		t.Fatalf("truncated stream was not signalled in-band: %d %s", resp.StatusCode, raw)
	}
	env := h.sink.last(t)
	if env.Outcome != "failure" || !env.Committed || len(env.Attempts) != 1 || h.mock.count("b") != 0 {
		t.Fatalf("truncated stream outcome changed: %+v", env)
	}
	fact := env.Attempts[0]
	if fact.Class != classProtocol || fact.FaultOrigin != faultProviderTransport || fact.FaultScope != scopeEndpoint || fact.NativeStatus != "" {
		t.Fatalf("truncated stream lost provider transport attribution: %+v", fact)
	}
	stats := h.gateway.Health().ProviderHealth(time.Minute)
	if s := stats[fact.ProviderID]; s.TransportErrors != 1 {
		t.Fatalf("a truncated provider stream stopped charging endpoint health: %+v", s)
	}
}

// Proxy-local faults — here the gateway's own event byte bound — are not
// provider evidence: no matter how often they fire, the shared endpoint
// circuit never opens and sibling routes keep the provider eligible.
func TestProxyLocalFailuresDoNotOpenTheSharedProviderCircuit(t *testing.T) {
	cfg := Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 512}
	h := newHarness(t, cfg)
	oversized := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"piece\"},\"finish_reason\":null}]}\n\n")
		io.WriteString(w, "data: "+strings.Repeat("x", int(cfg.MaxEventBytes))+"\n\n")
	}
	h.mock.set("a", oversized)
	for i := 0; i < circuitFailures; i++ {
		resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "proxy_resource_exhausted") {
			t.Fatalf("proxy-local fault was not reported as gateway capacity: %d %v %s", resp.StatusCode, err, raw)
		}
	}
	if h.mock.count("a") != circuitFailures {
		t.Fatalf("provider a called %d times", h.mock.count("a"))
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 || env.Attempts[0].Class != classProtocol || env.Attempts[0].FaultOrigin != faultProxyCapacity || env.Attempts[0].FaultScope != scopeRequest {
		t.Fatalf("proxy capacity fault was not attributed to the gateway: %+v", env.Attempts)
	}
	providerID := env.Attempts[0].ProviderID
	stats := h.gateway.Health().ProviderHealth(time.Minute)
	if s := stats[providerID]; s.TransportErrors != 0 || s.ServerErrors != 0 {
		t.Fatalf("proxy-local faults charged the provider circuit: %+v", s)
	}
	if h.gateway.health.open(providerID) {
		t.Fatal("proxy-local faults opened the shared provider circuit")
	}
	// A second route over the same provider stays eligible; the next attempt
	// reaches provider a rather than skipping an open circuit.
	route := h.rt.release.Snapshot.Routes[routeSlug]
	route.ID, route.Slug, route.RoutingID, route.RevisionID = uuid.NewString(), "another-route", uuid.NewString(), uuid.NewString()
	for i := range route.Targets {
		route.Targets[i].RoutingID = uuid.NewString()
	}
	h.rt.release.Snapshot.Routes[route.Slug] = route
	h.mock.set("a", completion(modelA, answerText))
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", otherKey, []byte(`{"model":"another-route","messages":[{"role":"user","content":"hi"}]}`), nil)
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), answerText) {
		t.Fatalf("sibling route lost provider eligibility: %d %v %s", resp.StatusCode, err, raw)
	}
	if h.mock.count("a") != circuitFailures+1 {
		t.Fatalf("provider a was excluded by proxy-local evidence: %d calls", h.mock.count("a"))
	}
}
