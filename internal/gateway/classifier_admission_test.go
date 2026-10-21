package gateway

import (
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"
)

// Both one-token prompts retain their 4096-token output allowance; the
// provider's five-token replies cannot refund either local reservation.

func TestNativeClassifierPredicateReturnsLabelsAndFallsThroughOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body, provider string
		status               int
	}{
		{"complex", `[{"label":"simple","score":0.1},{"label":"complex","score":0.9}]`, "b", http.StatusOK},
		{"simple", `[{"label":"simple","score":0.9}]`, "a", http.StatusOK},
		{"failure", `{"error":"unavailable"}`, "a", http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
				triage(s, b, routing.Predicate{Classifier: &routing.ClassifierPredicate{Route: classifierSlug, Labels: []string{"complex"}, TimeoutMS: 1000}})
				classifier := b
				classifier.ID, classifier.Name = uuid.NewString(), "c"
				classifier.Endpoint = h.upstream.URL + "/c/v1"
				classifier.ProfileID, classifier.ProfileRevision = "tei-classification", "1"
				classifier.Capabilities = []routing.Capability{{Model: modelB, Operation: "classification", Surface: "native", Mode: "unary"}}
				s.Providers[classifier.ID] = classifier
				route := s.Routes[classifierSlug]
				route.Operations = []string{"classification"}
				route.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict}
				route.Targets[0].ProviderID = classifier.ID
				s.Routes[classifierSlug] = route
			})
			h.mock.set("c", status(tc.status, tc.body))
			resp, body := h.chat(fullKey, nil)
			if resp.StatusCode != http.StatusOK || h.mock.count("c") != 1 || h.mock.count(tc.provider) != 1 {
				t.Fatalf("status=%d body=%v classifier=%d selected=%d", resp.StatusCode, body, h.mock.count("c"), h.mock.count(tc.provider))
			}
		})
	}
}
