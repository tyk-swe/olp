package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/usage"
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

func TestPinnedTransportEvidenceDistinguishesBeforeAndAfterTransmission(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(map[bool]string{false: "before transmission", true: "after transmission"}[sent], func(t *testing.T) {
			h, x, p := pinnedExchangeFixture(t)
			h.gateway.client = &http.Client{Transport: pinnedTransportFunc(func(r *http.Request) (*http.Response, error) {
				if sent {
					httptrace.ContextClientTrace(r.Context()).WroteHeaders()
				}
				return nil, io.ErrUnexpectedEOF
			})}
			_, failure := h.gateway.pinnedDo(t.Context(), x, p, http.MethodPost, p.provider.Endpoint+"/interactions", nil, "")
			if failure == nil || failure.dispatched != sent || x.dispatched != sent {
				t.Fatalf("lost transport evidence: %+v, %v", failure, x.dispatched)
			}
			fact := x.facts[0]
			if fact.Committed || fact.BillingUncertain != sent {
				t.Fatalf("untruthful final fact: %+v", fact)
			}
			want := classConnect
			if sent {
				want = classAmbiguous
			}
			if failure.class != want {
				t.Fatalf("class=%s want %s", failure.class, want)
			}
		})
	}
}

func TestPinnedSuccessfulHeadersAreAcceptedWithoutCompletionOrCommitment(t *testing.T) {
	h, x, p := pinnedExchangeFixture(t)
	h.gateway.client = &http.Client{Transport: pinnedTransportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":`)), Request: r}, nil
	})}
	resp, failure := h.gateway.pinnedDo(t.Context(), x, p, http.MethodPost, p.provider.Endpoint+"/interactions", nil, "")
	if failure != nil {
		t.Fatal(failure)
	}
	defer resp.Body.Close()
	if !x.dispatched || x.facts[0].Class == classSuccess || x.facts[0].Committed {
		t.Fatalf("headers became completion: %+v", x.facts[0])
	}
	// A refused/oversized/truncated body leaves the accepted work unresolved.
	x.failure = serverError(502, "provider_protocol_error", "invalid provider body")
	h.gateway.finish(x, nil, 502)
	if !x.facts[0].BillingUncertain || x.facts[0].UsageComplete {
		t.Fatalf("accepted invalid body was released as free: %+v", x.facts[0])
	}
}

func TestPinnedObservedUsageSurvivesMappingAndDeliveryFailure(t *testing.T) {
	for _, delivery := range []bool{false, true} {
		t.Run(map[bool]string{false: "mapping failure", true: "delivery failure"}[delivery], func(t *testing.T) {
			h, x, p := pinnedExchangeFixture(t)
			x.exchange = h.gateway.beginPinned(t.Context(), x, p, http.MethodPost)
			x.exchange.state.dispatched.Store(true)
			x.exchange.state.accepted, x.dispatched = true, true
			x.facts[0].Interaction = &usage.InteractionEvidence{UpstreamState: usage.UpstreamAccepted}
			x.facts[0].Usage = &wire.Usage{InputTokens: 9, OutputTokens: 5}
			x.completePinned()
			if delivery {
				h.gateway.writeStateJSON(&failedMediaWriter{header: make(http.Header)}, x, []byte(`{"id":"owned"}`))
			} else {
				x.failure = serverError(503, "provider_state_unavailable", "mapping could not commit")
				h.gateway.finish(x, nil, 503)
			}
			fact := x.facts[0]
			if fact.Usage == nil || fact.Usage.InputTokens != 9 || !fact.UsageComplete || fact.BillingUncertain || fact.Interaction.UpstreamState != usage.UpstreamTerminal {
				t.Fatalf("provider observation lost: %+v", fact)
			}
			if delivery && (x.failure.Code != "client_delivery_failed" || !fact.Committed || fact.Class != classCancelled) {
				t.Fatalf("failed write reported success: %+v, %+v", x.failure, fact)
			}
			if !delivery && (fact.Committed || fact.Class != classSuccess) {
				t.Fatalf("local persistence overwrote upstream completion: %+v", fact)
			}
		})
	}
}

func TestPinnedCancellationBeforeTransmissionDoesNotCharge(t *testing.T) {
	h, x, p := pinnedExchangeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h.gateway.client = &http.Client{Transport: pinnedTransportFunc(func(*http.Request) (*http.Response, error) { return nil, context.Canceled })}
	_, failure := h.gateway.pinnedDo(ctx, x, p, http.MethodPost, p.provider.Endpoint+"/interactions", nil, "")
	if failure == nil || failure.dispatched || x.dispatched || x.facts[0].BillingUncertain || failure.class != classCancelled {
		t.Fatalf("unsent cancellation charged: %+v, %+v", failure, x.facts[0])
	}
}

type pinnedFlushFailure struct{ header http.Header }

func (w *pinnedFlushFailure) Header() http.Header              { return w.header }
func (w *pinnedFlushFailure) WriteHeader(int)                  {}
func (w *pinnedFlushFailure) Write(b []byte) (int, error)      { return len(b), nil }
func (w *pinnedFlushFailure) SetWriteDeadline(time.Time) error { return nil }
func (w *pinnedFlushFailure) FlushError() error                { return io.ErrClosedPipe }

func TestPinnedBufferedFlushFailurePreservesUpstreamCompletion(t *testing.T) {
	h, x, p := pinnedExchangeFixture(t)
	e := h.gateway.beginPinned(t.Context(), x, p, http.MethodPost)
	e.state.dispatched.Store(true)
	e.state.accepted, x.dispatched = true, true
	x.facts[0].Usage = &wire.Usage{InputTokens: 9, OutputTokens: 5}
	x.completePinned()
	h.gateway.writeStateJSON(&pinnedFlushFailure{header: make(http.Header)}, x, []byte(`{"id":"owned"}`))
	fact := x.facts[0]
	if x.failure == nil || x.failure.Code != "client_delivery_failed" || fact.Class != classCancelled || !fact.Committed {
		t.Fatalf("buffer acceptance became successful delivery: failure=%v fact=%+v", x.failure, fact)
	}
	if !e.state.settled || !fact.UsageComplete || fact.BillingUncertain || fact.Usage.InputTokens != 9 {
		t.Fatalf("flush failure overwrote provider result: %+v", fact)
	}
	if x.firstByte != nil {
		t.Fatal("buffered bytes were reported delivered before the failed flush")
	}
}
