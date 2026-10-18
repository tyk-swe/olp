package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestCompletionProfilesUseNativeHealthProbe(t *testing.T) {
	for _, vendor := range []string{"openai", "tgi"} {
		t.Run(vendor, func(t *testing.T) {
			h := strictHarness(t, func(s *routing.Snapshot) {
				route := s.Routes[routeSlug]
				route.Fidelity.Mode = routing.FidelityTransformed
				s.Routes[routeSlug] = route
				for id, p := range s.Providers {
					p.Kind = "openai_compatible"
					if vendor == "openai" {
						p.Kind = "openai"
					}
					p.VendorID = vendor
					p.ProfileID = vendor + "-completions"
					s.Providers[id] = p
				}
			})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/a/v1/completions" || body["prompt"] == nil || body["messages"] != nil {
					t.Errorf("probe used wrong dialect: %s %v", r.URL.Path, body)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"c","object":"text_completion","model":"`+modelA+`","choices":[{"index":0,"text":"OK","finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
			})
			route := h.rt.release.Snapshot.Routes[routeSlug]
			target := route.Targets[0]
			result, ok := h.gateway.probe(t.Context(), h.rt.release, probeTarget{route: route, target: target, provider: target.ProviderID, operation: operationGeneration})
			if !ok || result.Status != limits.ProbeHealthy || h.mock.count("a") != 1 {
				t.Fatalf("native probe did not report health: %+v ok=%v", result, ok)
			}
		})
	}
}
