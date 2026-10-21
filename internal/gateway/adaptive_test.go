package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const backupSlug = "team-backup"

// republish installs a release whose snapshot edit has changed. The harness
// route serves provider a first and b second; edit may reshape it and add
// routes of its own.
func (h *harness) republish(edit func(s *routing.Snapshot, a, b routing.Provider)) {
	h.t.Helper()
	old := h.rt.Release()
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: old.Snapshot.Generation.Ordinal + 1, ActivatedAt: old.Snapshot.Generation.ActivatedAt},
		Providers:  maps.Clone(old.Snapshot.Providers),
		Routes:     maps.Clone(old.Snapshot.Routes),
	}
	var a, b routing.Provider
	credentials := map[string][]byte{}
	for _, provider := range snapshot.Providers {
		switch provider.Name {
		case "a":
			a = provider
		case "b":
			b = provider
		}
		if secret, ok := old.Credential(*provider.ActiveCredential); ok {
			credentials[*provider.ActiveCredential] = secret
		}
	}
	edit(snapshot, a, b)
	release, err := runtime.NewRelease(uuid.NewString(), old.Sequence+1, snapshot, credentials)
	if err != nil {
		h.t.Fatal(err)
	}
	h.rt.mu.Lock()
	h.rt.release = release
	h.rt.mu.Unlock()
}

// provider resolves a harness provider's ID by name.
func (h *harness) provider(name string) string {
	h.t.Helper()
	for _, provider := range h.rt.Release().Snapshot.Providers {
		if provider.Name == name {
			return provider.ID
		}
	}
	h.t.Fatalf("no provider %s", name)
	return ""
}

// splitRoutes leaves the harness route serving only provider a and adds a
// backup route that serves only provider b.
func splitRoutes(s *routing.Snapshot, b routing.Provider, behavior routing.Behavior) {
	primary := s.Routes[routeSlug]
	backup := primary
	primary.Targets = primary.Targets[:1]
	primary.Behavior = behavior
	backupID := uuid.NewString()
	backup.ID, backup.RoutingID, backup.RevisionID, backup.Slug = backupID, backupID, uuid.NewString(), backupSlug
	backup.Targets = []routing.Target{{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()}}
	s.Routes[routeSlug], s.Routes[backupSlug] = primary, backup
}

func TestContentFilterRefusalFallsBackWithoutTouchingHealth(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *routing.Snapshot, _, b routing.Provider) {
		splitRoutes(s, b, routing.Behavior{Fallbacks: []routing.Fallback{{Route: backupSlug, On: []string{routing.FallbackContentFilter}}}})
	})
	h.mock.set("a", status(http.StatusBadRequest, `{"error":{"message":"refused","type":"invalid_request_error","code":"content_filter"}}`))
	for range circuitFailures + 1 {
		if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
			t.Fatal(resp.Status)
		}
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[0].Class != classContentFilter {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	if h.gateway.OpenCircuits() != 0 || h.mock.count("a") != circuitFailures+1 {
		t.Fatalf("a content filter opened a circuit: open %d, calls to a %d", h.gateway.OpenCircuits(), h.mock.count("a"))
	}
	// Without a content_filter fallback the refusal reaches the caller.
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Fallbacks = nil
		s.Routes[routeSlug] = route
	})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "content_filter" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
}

// Retries are bounded by the rule and consume the attempt budget; the
// route then fails over with what remains.

// Each attempt records which repeat of its slot it was.

// A shadow's failure never reaches the caller.

func TestSaturatedGatewayQueuesRequestsByPriority(t *testing.T) {
	pool := observability.NewPool(1).Queue(2, 5*time.Second)
	h := newHarness(t, Config{MaxInFlight: 1, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096, AdmissionPool: pool})
	authority := h.rt.keys[fullKey]
	authority.Policy.MaxPriority = new("critical")
	h.rt.keys[fullKey] = authority
	admission := &observability.PublicAdmission{Inference: pool, Management: observability.NewPool(1), InferenceEnabled: true,
		Reject: func(w http.ResponseWriter, r *http.Request, surface string) { writeError(w, overloaded) }}
	mux := http.NewServeMux()
	h.gateway.Register(mux)
	server := httptest.NewServer(admission.Wrap(mux))
	t.Cleanup(server.Close)

	hold := make(chan struct{})
	var (
		mu     sync.Mutex
		served []string
	)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		var request struct{ User string }
		json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		served = append(served, request.User)
		mu.Unlock()
		if request.User == "first" {
			<-hold
		}
		completion(modelA, answerText)(w, r)
	})
	send := func(user, class string) <-chan int {
		statuses := make(chan int, 1)
		go func() {
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"user":"`+user+`"}`))
			req.Header.Set("Authorization", "Bearer "+fullKey)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(routingHeader, `{"priority":"`+class+`"}`)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
		return statuses
	}
	queued := func(class string, want int) {
		t.Helper()
		line := fmt.Sprintf("olp_admission_queue_depth{class=%q} %d\n", class, want)
		for deadline := time.Now().Add(5 * time.Second); ; {
			var body strings.Builder
			observability.AdmissionMetrics(pool, observability.NewPool(1), &body)
			if strings.Contains(body.String(), line) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %s request waited in the queue:\n%s", class, body.String())
			}
			time.Sleep(time.Millisecond)
		}
	}

	first := send("first", "normal")
	for h.mock.count("a") == 0 {
		time.Sleep(time.Millisecond)
	}
	low := send("low", "low")
	queued("low", 1)
	critical := send("critical", "critical")
	queued("critical", 1)
	// The queue is full: a further request is refused as before.
	if status := <-send("excess", "critical"); status != http.StatusServiceUnavailable {
		t.Fatalf("a request beyond the queue depth got %d, want 503", status)
	}
	close(hold)
	for _, statuses := range []<-chan int{first, critical, low} {
		if status := <-statuses; status != http.StatusOK {
			t.Fatalf("a queued request got %d", status)
		}
	}
	if want := []string{"first", "critical", "low"}; !slices.Equal(served, want) {
		t.Fatalf("served %v, want %v", served, want)
	}
}

const classifierSlug = "team-classifier"

// triage tags provider a simple and provider b complex, adds a classifier
// route that b serves, and routes complex requests by the given predicate.
func triage(s *routing.Snapshot, b routing.Provider, when routing.Predicate) {
	route := s.Routes[routeSlug]
	route.Targets[0].Tags, route.Targets[1].Tags = []string{"simple"}, []string{"complex"}
	route.Selectors = []routing.Selector{
		{ID: "hard", When: when, Tags: []string{"complex"}},
		{ID: "easy", Tags: []string{"simple"}},
	}
	classifier := route
	classifier.Behavior = routing.Behavior{}
	id := uuid.NewString()
	classifier.ID, classifier.RoutingID, classifier.RevisionID, classifier.Slug = id, id, uuid.NewString(), classifierSlug
	classifier.Targets = []routing.Target{{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()}}
	s.Routes[routeSlug], s.Routes[classifierSlug] = route, classifier
}

// predicates answers plugin predicates in-process.
type predicates func(digest string, input abi.RoutePredicate) (bool, error)

func (p predicates) RoutePredicate(_ context.Context, digest string, input abi.RoutePredicate) (bool, error) {
	return p(digest, input)
}

// pipelining is a limiter that admits everything and counts its round trips:
// each Do and each Pipeline is one.
type pipelining struct {
	*allowing
	pipelines atomic.Int64
}

func (p *pipelining) Pipeline(_ context.Context, commands ...[]string) ([]any, error) {
	p.pipelines.Add(1)
	replies := make([]any, len(commands))
	for i := range replies {
		replies[i] = []any{int64(i), int64(0), int64(0)}
	}
	return replies, nil
}

func (p *pipelining) trips() int64 { return p.calls.Load() + p.pipelines.Load() }

// The capacity strategy reads every candidate slot's headroom in one pipelined
// round trip, and costs nothing beyond it.
func TestCapacityStrategyAddsOneValkeyRoundTrip(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *routing.Snapshot, a, b routing.Provider) {
		for _, provider := range []routing.Provider{a, b} {
			provider.Limits = &routing.Limits{MaxConcurrency: int64ptr(8)}
			s.Providers[provider.ID] = provider
		}
	})
	client := &pipelining{allowing: newAllowing(0, 0, 0, true)}
	limiter, err := limits.New(client, "olp:test:limits")
	if err != nil {
		t.Fatal(err)
	}
	h.gateway.Admission = NewAdmission(limiter, nil, quiet)
	h.mock.set("a", completion(modelA, answerText))
	h.mock.set("b", completion(modelB, answerText))

	served := func(strategy string) int64 {
		before := client.trips()
		if resp, body := h.chat(fullKey, map[string]string{routingHeader: `{"strategy":"` + strategy + `"}`}); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %v", strategy, resp.StatusCode, body)
		}
		return client.trips() - before
	}
	weighted, capacity := served("weighted"), served("capacity")
	if client.pipelines.Load() != 1 || capacity != weighted+1 {
		t.Fatalf("weighted took %d round trips, capacity %d with %d pipelines", weighted, capacity, client.pipelines.Load())
	}
}
