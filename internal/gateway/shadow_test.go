package gateway

import (
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func TestShadowPanicLeavesThePrimaryResponseIntact(t *testing.T) {
	h := newHarness(t, Config{MaxShadowInFlight: 1})
	var shadowCredential string
	h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
		route := s.Routes[routeSlug]
		route.Targets = slices.Clone(route.Targets)
		route.Targets[1].Shadow = &routing.Shadow{SampleRate: 1}
		s.Routes[routeSlug] = route
		shadowCredential = *b.ActiveCredential
	})
	var panicked atomic.Bool
	h.rt.mu.Lock()
	h.rt.onSecret = func(id string) {
		if id == shadowCredential {
			panicked.Store(true)
			panic("credential store fault")
		}
	}
	h.rt.mu.Unlock()

	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("primary status = %d, body = %v", resp.StatusCode, body)
	}
	if err := h.gateway.DrainShadows(t.Context()); err != nil {
		t.Fatalf("drain shadows: %v", err)
	}
	if !panicked.Load() {
		t.Fatal("the shadow never reached its credential lookup")
	}
}
