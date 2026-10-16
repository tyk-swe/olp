//go:build integration

package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"

	"github.com/tyk-swe/olp/internal/egress"
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

// Provider a fails on the first replica until its circuit opens; b, the
// lower-priority target, serves every request meanwhile.

// The other replica never saw a failure; the shared circuit reaches it
// within the staleness bound.

// It orders a's target last, so b serves at the first attempt and a
// receives nothing.

// The probe is an accounted request of the installation's own: no key,
// origin probe, one attempt.

// The verdict opens the provider's fleet circuit until the next probe.

// A claim covers the whole interval, so no replica probes it again early.

// Once the interval passes, a healthy probe closes the circuit for every
// replica.
