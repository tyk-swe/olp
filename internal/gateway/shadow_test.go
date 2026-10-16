package gateway

import (
	"maps"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestShadowCarriesTheCallersAttribution(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		s.Routes[routeSlug].Targets[1].Shadow = &routing.Shadow{SampleRate: 1}
	})
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowedAttributionKeys = []string{"team"}
	h.rt.keys[fullKey] = authority
	h.mock.set("a", completion(modelA, answerText))
	h.mock.set("b", completion(modelB, answerText))
	if resp, body := h.chat(fullKey, map[string]string{attribution.Header: `{"team":"core"}`}); resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s", resp.Status, body)
	}
	h.gateway.WaitShadows()
	h.sink.mu.Lock()
	envs := append([]Envelope(nil), h.sink.envs...)
	h.sink.mu.Unlock()
	if len(envs) != 2 || envs[1].Origin != usage.OriginShadow {
		t.Fatalf("shadow not recorded: %+v", envs)
	}
	// The mirror's spend is held and charged under the labels the caller's was.
	if !maps.Equal(envs[1].Attribution, map[string]string{"team": "core"}) {
		t.Fatalf("shadow attribution = %v", envs[1].Attribution)
	}
}
