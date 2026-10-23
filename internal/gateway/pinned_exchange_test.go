package gateway

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

func pinnedExchangeFixture(t *testing.T) (*harness, *call, *pin) {
	t.Helper()
	h := newHarness(t, Config{})
	var provider routing.Provider
	for _, candidate := range h.rt.release.Snapshot.Providers {
		if candidate.Name == "a" {
			provider = candidate
		}
	}
	route := h.rt.release.Snapshot.Routes[routeSlug]
	x := &call{request: request{release: h.rt.release, startedAt: time.Now()}, family: wire.GeminiInteractions, mode: "unary", estimate: 100, routeLeg: routeLeg{route: &route}}
	p := &pin{provider: provider, slot: provider.Slots[0], model: modelA, attempt: routing.Attempt{ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, UpstreamModel: modelA}}
	return h, x, p
}

// A refused/oversized/truncated body leaves the accepted work unresolved.
