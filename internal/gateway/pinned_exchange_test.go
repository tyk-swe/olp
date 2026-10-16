package gateway

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

type pinnedTransportFunc func(*http.Request) (*http.Response, error)

func (f pinnedTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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

type pinnedFlushFailure struct{ header http.Header }

func (w *pinnedFlushFailure) Header() http.Header              { return w.header }
func (w *pinnedFlushFailure) WriteHeader(int)                  {}
func (w *pinnedFlushFailure) Write(b []byte) (int, error)      { return len(b), nil }
func (w *pinnedFlushFailure) SetWriteDeadline(time.Time) error { return nil }
func (w *pinnedFlushFailure) FlushError() error                { return io.ErrClosedPipe }
