package gateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestClassifierDoesNotDispatchForAnExhaustedCaller(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		triage(s, b, runtime.Predicate{Classifier: &runtime.ClassifierPredicate{Route: classifierSlug, Labels: []string{"complex"}, TimeoutMS: 1000}})
	})
	server := admitThrough(t, h)
	key := h.rt.keys[fullKey]
	key.LookupID, key.Policy.RequestsPerMinute = "classifier", costCount(1)
	h.rt.keys[fullKey] = key
	server.of("olp:test:{classifier}:rate").requests = 1
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusTooManyRequests || h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatalf("exhausted caller dispatched: status=%d body=%v a=%d b=%d", resp.StatusCode, body, h.mock.count("a"), h.mock.count("b"))
	}
}

func TestClassifierSettlesItsOwnCallerReservation(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		triage(s, b, runtime.Predicate{Classifier: &runtime.ClassifierPredicate{Route: classifierSlug, Labels: []string{"complex"}, TimeoutMS: 1000}})
	})
	server := admitThrough(t, h)
	key := h.rt.keys[fullKey]
	key.LookupID, key.Policy.RequestsPerMinute, key.Policy.TokensPerMinute = "classifier", costCount(10), costCount(100_000)
	h.rt.keys[fullKey] = key
	h.mock.set("b", completion(modelB, "simple"))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	server.settled(t, 2)
	server.mu.Lock()
	defer server.mu.Unlock()
	counter := server.of("olp:test:{classifier}:rate")
	// Both one-token prompts retain their 4096-token output allowance; the
	// provider's five-token replies cannot refund either local reservation.
	if counter.requests != 2 || counter.tokens != 8194 || server.reconciled != 2 {
		t.Fatalf("caller and classifier reservations: counter=%+v reconciliations=%d", counter, server.reconciled)
	}
}

func TestClassifierWaitsForTheCallersQueuePermit(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		triage(s, b, runtime.Predicate{Classifier: &runtime.ClassifierPredicate{Route: classifierSlug, Labels: []string{"complex"}, TimeoutMS: 1000}})
	})
	parsed, err := protocols.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hi"}]}`), routeSlug)
	if err != nil {
		t.Fatal(err)
	}
	parent := &execution{
		request: request{id: uuid.NewString(), minted: true, release: h.rt.Release(), startedAt: time.Now()},
		parsed:  parsed, family: openai.FamilyChat, mode: "unary", authority: h.rt.keys[fullKey],
	}
	pool := observability.NewPool(1)
	pool.Queue(1, time.Second)
	held := pool.Enter()
	defer held.Release()
	queued := pool.Enter()
	defer queued.Release()
	ctx, cancel := context.WithTimeout(observability.WithPermit(t.Context(), queued), 20*time.Millisecond)
	defer cancel()
	result := h.gateway.callClassifier(ctx, parent, &runtime.ClassifierPredicate{Route: classifierSlug, TimeoutMS: 1000})
	if result.failure == "" || h.mock.count("b") != 0 {
		t.Fatalf("queued classifier dispatched: result=%+v calls=%d", result, h.mock.count("b"))
	}
}

func TestClassifierSharesTheNamedRoutesOverallDeadline(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		triage(s, b, runtime.Predicate{Classifier: &runtime.ClassifierPredicate{Route: classifierSlug, Labels: []string{"complex"}, TimeoutMS: 1000}})
		route := s.Routes[routeSlug]
		route.OverallTimeout = 50
		s.Routes[routeSlug] = route
	})
	h.mock.set("a", completion(modelA, answerText))
	h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(250 * time.Millisecond):
			completion(modelB, "complex")(w, r)
		}
	})
	started := time.Now()
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode == http.StatusOK || time.Since(started) >= 200*time.Millisecond {
		t.Fatalf("classifier escaped the route deadline: status=%d elapsed=%s body=%s", resp.StatusCode, time.Since(started), body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 1 {
		t.Fatalf("dispatches beyond deadline: %d/%d", h.mock.count("a"), h.mock.count("b"))
	}
}

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
			h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
				triage(s, b, runtime.Predicate{Classifier: &runtime.ClassifierPredicate{Route: classifierSlug, Labels: []string{"complex"}, TimeoutMS: 1000}})
				classifier := b
				classifier.ID, classifier.Name = uuid.NewString(), "c"
				classifier.Endpoint = h.upstream.URL + "/c/v1"
				classifier.ProfileID, classifier.ProfileRevision = "tei-classification", "1"
				classifier.Capabilities = []runtime.Capability{{Model: modelB, Operation: "classification", Surface: "native", Mode: "unary"}}
				s.Providers[classifier.ID] = classifier
				route := s.Routes[classifierSlug]
				route.Operations = []string{"classification"}
				route.Fidelity = runtime.RouteFidelity{Mode: runtime.FidelityStrict}
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
