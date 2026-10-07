//go:build integration

package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// replica is a second gateway serving the harness's runtime with circuits of
// its own, as another replica of the fleet does.
func (h *harness) replica() (*Server, *capture, *httptest.Server) {
	h.t.Helper()
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	gw := New(h.rt, &policy, Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sink := &capture{}
	gw.Sink = sink
	mux := http.NewServeMux()
	gw.Register(mux)
	server := httptest.NewServer(mux)
	h.t.Cleanup(server.Close)
	return gw, sink, server
}

func TestCircuitOpenedOnOneGatewayIsHonoredByAnother(t *testing.T) {
	h := newHarness(t, Config{})
	limiter := mediaLimiter(t)
	log := slog.New(slog.DiscardHandler)
	other, otherSink, otherServer := h.replica()
	h.gateway.Admission = NewAdmission(limiter, nil, log)
	other.Admission = NewAdmission(limiter, nil, log)
	go h.gateway.RunFleetHealth(t.Context())
	go other.RunFleetHealth(t.Context())

	// Provider a fails on the first replica until its circuit opens; b, the
	// lower-priority target, serves every request meanwhile.
	h.mock.set("a", status(http.StatusInternalServerError, `{"error":{"message":"down"}}`))
	for range circuitFailures {
		if resp, body := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want failover to b: %v", resp.StatusCode, body)
		}
	}
	if h.gateway.OpenCircuits() != 1 {
		t.Fatalf("first replica opened %d circuits, want 1", h.gateway.OpenCircuits())
	}
	opened := time.Now()

	// The other replica never saw a failure; the shared circuit reaches it
	// within the staleness bound.
	provider := h.rt.release.Snapshot.Routes[routeSlug].Targets[0].ProviderID
	for !other.fleet.unhealthy(provider) {
		if time.Since(opened) > fleetStaleness {
			t.Fatalf("the shared circuit did not reach the other replica within %s", fleetStaleness)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// It orders a's target last, so b serves at the first attempt and a
	// receives nothing.
	served := h.mock.count("a")
	h.server = otherServer
	for range 3 {
		if resp, body := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %v", resp.StatusCode, body)
		}
		if attempts := otherSink.last(t).Attempts; len(attempts) != 1 || attempts[0].ProviderID == provider {
			t.Fatalf("attempts = %+v, want one attempt on b", attempts)
		}
	}
	if h.mock.count("a") != served {
		t.Fatal("the other replica dispatched to a provider the fleet marked unhealthy")
	}
	if other.OpenCircuits() != 0 {
		t.Fatal("the other replica opened a local circuit of its own")
	}
}

func TestActiveProbesShareTheirVerdictWithTheFleet(t *testing.T) {
	h := newHarness(t, Config{})
	client, namespace := mediaValkey(t)
	limiter, err := limits.New(client, namespace)
	if err != nil {
		t.Fatal(err)
	}
	h.gateway.Admission = NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	h.republish(func(s *runtime.Snapshot, a, _ runtime.Provider) {
		a.HealthProbe = &runtime.HealthProbe{IntervalSeconds: 60}
		s.Providers[a.ID] = a
	})
	provider := h.rt.Release().Snapshot.Routes[routeSlug].Targets[0].ProviderID
	h.mock.set("a", status(http.StatusInternalServerError, `{"error":{"message":"down"}}`))

	probed, err := h.gateway.probeDue(t.Context())
	if err != nil || probed != 1 {
		t.Fatalf("probed = %d, %v; want the one opted-in provider", probed, err)
	}
	if h.mock.count("b") != 0 {
		t.Fatal("a provider without probing enabled was probed")
	}

	// The probe is an accounted request of the installation's own: no key,
	// origin probe, one attempt.
	env := h.sink.last(t)
	if env.Origin != usage.OriginProbe || env.KeyID != "" || len(env.Attempts) != 1 {
		t.Fatalf("probe envelope = origin %q key %q attempts %d", env.Origin, env.KeyID, len(env.Attempts))
	}
	if event := accountingEvent(env); event == nil || event.APIKeyID != "" || event.Origin != usage.OriginProbe {
		t.Fatalf("probe usage event = %+v", event)
	}

	// The verdict opens the provider's fleet circuit until the next probe.
	probes, err := limiter.Probes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result := probes[provider]; result.Status != limits.ProbeUnhealthy || result.Class != classUpstreamServer {
		t.Fatalf("probe result = %+v", result)
	}
	open, err := limiter.Circuits(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if until, ok := open[provider]; !ok || time.Until(until) < 50*time.Second {
		t.Fatalf("fleet circuits = %v, want the probed provider open for the probe interval", open)
	}

	// A claim covers the whole interval, so no replica probes it again early.
	if probed, err := h.gateway.probeDue(t.Context()); err != nil || probed != 0 {
		t.Fatalf("second pass probed %d, %v; want the claim to hold", probed, err)
	}

	// Once the interval passes, a healthy probe closes the circuit for every
	// replica.
	if _, err := client.Do(t.Context(), "DEL", namespace+":health:probe:"+provider+"/"+modelA); err != nil {
		t.Fatal(err)
	}
	h.mock.set("a", completion(modelA, answerText))
	if probed, err := h.gateway.probeDue(t.Context()); err != nil || probed != 1 {
		t.Fatalf("third pass probed %d, %v", probed, err)
	}
	if open, err := limiter.Circuits(t.Context(), time.Now()); err != nil || len(open) != 0 {
		t.Fatalf("fleet circuits = %v, %v; want the healthy probe to close them", open, err)
	}
}
