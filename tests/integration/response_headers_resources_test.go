//go:build integration

package integration_test

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
)

// rhLimiter is the Valkey limiter a harness's gateway admits through, with the
// namespace its windows live in. The harness has none of its own: without one a
// key's limits are not counted, and a response has no allowance to state, which
// would make a check that it states none say nothing.
type rhLimiter struct {
	client    *coordination.Client
	namespace string
}

// rhLimit puts a limiter in front of the harness's gateway.
func rhLimit(t *testing.T, h *accessHarness, label string) rhLimiter {
	t.Helper()
	c := limClient(t)
	namespace := limNamespace(t, c, label)
	h.Gateway.Admission = gateway.NewAdmission(limLimiter(t, c, namespace), func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
	return rhLimiter{c, namespace}
}

// counted is the requests the limiter has counted in a key's window.
func (l rhLimiter) counted(t *testing.T, secret string) int64 {
	t.Helper()
	rateKey, _ := limRateKeys(l.namespace, strings.SplitN(secret, "_", 3)[1])
	return limField(t, limHash(t, l.client, rateKey), "rpm")
}

// rhEnvelope waits for the terminal envelope of the request that has just been
// answered, which the gateway emits as it finishes the response.
func rhEnvelope(t *testing.T, sink *captureSink, before int) gateway.Envelope {
	t.Helper()
	glEventually(t, "the request's terminal envelope", func() bool { return sink.count() > before })
	return sink.last()
}

// rhWantServedBy holds the metadata headers of a response to the envelope the
// gateway recorded for it: the attempts it made, the route revision and the
// vendor of the attempt that served it. A request that made no attempt has no
// metadata. None of these fixtures has a price list, so none states a cost.
func rhWantServedBy(t *testing.T, name string, header http.Header, env gateway.Envelope) {
	t.Helper()
	if len(env.Attempts) == 0 {
		if got := rhMetadata(header); len(got) != 0 {
			t.Fatalf("%s: a request that made no attempt carried %v", name, got)
		}
		return
	}
	if env.RouteRevisionID == "" {
		t.Fatalf("%s: the envelope names no route revision: %+v", name, env)
	}
	last := env.Attempts[len(env.Attempts)-1]
	want := map[string]string{"X-Olp-Attempts": strconv.Itoa(len(env.Attempts)), "X-Olp-Route-Revision": env.RouteRevisionID}
	if last.VendorID != "" {
		want["X-Olp-Provider"] = last.VendorID
	}
	got := rhMetadata(header)
	if len(got) != len(want) {
		t.Fatalf("%s: metadata = %v, want %v", name, got, want)
	}
	for header, value := range want {
		if got[header] != value {
			t.Fatalf("%s: %s = %q, want %q", name, header, got[header], value)
		}
	}
}

// A list is answered from the gateway's own records: no attempt, no metadata.

// Without the opt-in a response says nothing of how it was served.

// The opt-in is the key's policy, and takes effect on the next refresh.

// The requests are to be counted in one minute's window.

// The limiter counted the request, so the key has an allowance that
// a mis-resolved surface would state.

// The requests are to be counted in one minute's window.

// call makes one request as the opted-in key and holds the response to the
// attempts the gateway recorded for it.

// The limiter counted the request, so the key has an allowance that a
// mis-resolved surface would state.

// The calls on one Interaction run in order: a delete tombstones it, so every
// call that reads it comes first.

// A delete whose provider no longer has the Interaction, because an earlier
// delete whose response was lost already removed it, still succeeds, and still
// says it asked the provider.

// Both requests are to be counted in one minute's window.
