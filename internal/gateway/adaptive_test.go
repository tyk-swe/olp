package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const backupSlug = "team-backup"

// republish installs a release whose snapshot edit has changed. The harness
// route serves provider a first and b second; edit may reshape it and add
// routes of its own.
func (h *harness) republish(edit func(s *runtime.Snapshot, a, b runtime.Provider)) {
	h.t.Helper()
	old := h.rt.Release()
	snapshot := &runtime.Snapshot{
		Generation: runtime.Generation{ID: uuid.NewString(), Ordinal: old.Snapshot.Generation.Ordinal + 1, ActivatedAt: old.Snapshot.Generation.ActivatedAt},
		Providers:  maps.Clone(old.Snapshot.Providers),
		Routes:     maps.Clone(old.Snapshot.Routes),
	}
	var a, b runtime.Provider
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
func splitRoutes(s *runtime.Snapshot, b runtime.Provider, behavior runtime.Behavior) {
	primary := s.Routes[routeSlug]
	backup := primary
	primary.Targets = primary.Targets[:1]
	primary.Behavior = behavior
	backupID := uuid.NewString()
	backup.ID, backup.RoutingID, backup.RevisionID, backup.Slug = backupID, backupID, uuid.NewString(), backupSlug
	backup.Targets = []runtime.Target{{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()}}
	s.Routes[routeSlug], s.Routes[backupSlug] = primary, backup
}

func TestRetainedResponsesIgnoreRouteDelegatingSelectors(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		for id, provider := range s.Providers {
			provider.Kind, provider.Endpoint = "openai", ""
			s.Providers[id] = provider
		}
		splitRoutes(s, b, runtime.Behavior{Selectors: []runtime.Selector{{ID: "delegate", Route: backupSlug}}})
	})
	for _, retained := range []bool{false, true} {
		parsed, err := protocols.Parse(openai.FamilyResponses, []byte(`{"model":"team-chat","input":"hi","store":true}`), routeSlug)
		if err != nil {
			t.Fatal(err)
		}
		x := &execution{request: request{release: h.rt.Release()}, parsed: parsed, family: openai.FamilyResponses, mode: "unary", providerState: retained}
		if e := h.gateway.prepare(t.Context(), x, h.gateway.keyAuthorizer(h.rt.keys[fullKey])); e != nil {
			t.Fatal(e)
		}
		wantRoute, wantProvider := backupSlug, h.provider("b")
		if retained {
			wantRoute, wantProvider = routeSlug, h.provider("a")
		}
		if x.route.Slug != wantRoute || len(x.attempts) != 1 || x.attempts[0].ProviderID != wantProvider {
			t.Fatalf("retained=%t route=%s attempts=%+v", retained, x.route.Slug, x.attempts)
		}
	}
}

func TestFallbackRouteServesOnceThePrimaryIsExhausted(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		splitRoutes(s, b, runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: []string{runtime.FallbackExhausted}}}})
	})
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK || body["model"] != routeSlug {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	env := h.sink.last(t)
	if env.Route != routeSlug || len(env.Attempts) != 2 {
		t.Fatalf("envelope route %q attempts %+v", env.Route, env.Attempts)
	}
	first, second := env.Attempts[0], env.Attempts[1]
	if first.Leg != nil || first.Class != classUpstreamServer {
		t.Fatalf("primary attempt %+v", first)
	}
	if second.Class != classSuccess || second.Ordinal != 2 || second.Leg == nil ||
		second.Leg.Route != backupSlug || second.Leg.Via != usage.ViaFallback {
		t.Fatalf("fallback attempt %+v leg %+v", second, second.Leg)
	}
}

func TestFallbackFollowsOnlyTheConditionsItNames(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		splitRoutes(s, b, runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: []string{runtime.FallbackContextWindow}}}})
	})
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode == http.StatusOK || h.mock.count("b") != 0 {
		t.Fatalf("status %d, backup calls %d: a server failure is not a context-window failure", resp.StatusCode, h.mock.count("b"))
	}
	h.mock.set("a", status(http.StatusBadRequest, `{"error":{"message":"too long","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	resp, _ := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK || h.mock.count("b") != 1 {
		t.Fatalf("status %d, backup calls %d", resp.StatusCode, h.mock.count("b"))
	}
}

func TestContentFilterRefusalFallsBackWithoutTouchingHealth(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		splitRoutes(s, b, runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: []string{runtime.FallbackContentFilter}}}})
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
	h.republish(func(s *runtime.Snapshot, _, _ runtime.Provider) {
		route := s.Routes[routeSlug]
		route.Fallbacks = nil
		s.Routes[routeSlug] = route
	})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "content_filter" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
}

func TestFallbackNeverFollowsACommittedStream(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		splitRoutes(s, b, runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: runtime.FallbackConditions}}})
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"model-a","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hi"}],"stream":true}`), nil)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), `"content":"partial"`) {
		t.Fatalf("body %q", raw)
	}
	env := h.sink.last(t)
	if !env.Committed || len(env.Attempts) != 1 || h.mock.count("b") != 0 {
		t.Fatalf("a committed stream moved on: attempts %+v, backup calls %d", env.Attempts, h.mock.count("b"))
	}
}

func TestFallbackSkipsRoutesTheKeyMayNotUse(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		splitRoutes(s, b, runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: []string{runtime.FallbackExhausted}}}})
	})
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowedRoutes = []string{routeSlug}
	h.rt.keys[fullKey] = authority
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusBadGateway || errorCode(t, body) != "upstream_unavailable" || h.mock.count("b") != 0 {
		t.Fatalf("status %d body %v backup calls %d", resp.StatusCode, body, h.mock.count("b"))
	}
}

func TestFallbacksShareThePrimaryAttemptBudget(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		splitRoutes(s, b, runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: []string{runtime.FallbackExhausted}}}})
		primary := s.Routes[routeSlug]
		primary.MaxAttempts = 1
		s.Routes[routeSlug] = primary
	})
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode == http.StatusOK || h.mock.count("b") != 0 {
		t.Fatalf("status %d backup calls %d: the primary's one attempt was spent", resp.StatusCode, h.mock.count("b"))
	}
}

func TestRetryPolicyRepeatsTheSameSlotBeforeFailingOver(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, _ runtime.Provider) {
		route := s.Routes[routeSlug]
		route.Retry = runtime.Retry{classUpstreamServer: {MaxRetries: 1, BaseBackoffMS: 1, MaxBackoffMS: 2}}
		s.Routes[routeSlug] = route
	})
	var calls atomic.Int32
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			status(http.StatusBadGateway, `{"error":{"message":"blip","type":"server_error"}}`)(w, r)
			return
		}
		completion(modelA, answerText)(w, r)
	})
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("%s attempts %+v", resp.Status, h.sink.last(t).Attempts)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 2 || env.Attempts[1].Retry != 1 || env.Attempts[1].ProviderID != env.Attempts[0].ProviderID || h.mock.count("b") != 0 {
		t.Fatalf("attempts %+v, calls to b %d", env.Attempts, h.mock.count("b"))
	}
	// Retries are bounded by the rule and consume the attempt budget; the
	// route then fails over with what remains.
	h.mock.set("a", status(http.StatusBadGateway, `{"error":{"message":"down","type":"server_error"}}`))
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	env = h.sink.last(t)
	if len(env.Attempts) != 3 || env.Attempts[2].ProviderID == env.Attempts[0].ProviderID {
		t.Fatalf("attempts %+v", env.Attempts)
	}
	// Each attempt records which repeat of its slot it was.
	for i, want := range []int{0, 1, 0} {
		if env.Attempts[i].Retry != want {
			t.Fatalf("attempt %d recorded retry %d, want %d", i+1, env.Attempts[i].Retry, want)
		}
	}
}

func TestShadowTargetsMirrorTrafficAccountedToTheRoute(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, _ runtime.Provider) {
		route := s.Routes[routeSlug]
		route.Targets[1].Shadow = &runtime.Shadow{SampleRate: 1}
		s.Routes[routeSlug] = route
	})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK || body["model"] != routeSlug {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	h.gateway.WaitShadows()
	h.sink.mu.Lock()
	envs := append([]Envelope(nil), h.sink.envs...)
	h.sink.mu.Unlock()
	if len(envs) != 2 {
		t.Fatalf("envelopes %d, want the caller's and one mirror", len(envs))
	}
	caller, shadow := envs[0], envs[1]
	if caller.Origin != "" || caller.KeyID != h.keyID || len(caller.Attempts) != 1 {
		t.Fatalf("caller envelope %+v", caller)
	}
	if shadow.Origin != usage.OriginShadow || shadow.KeyID != "" || shadow.ParentRequestID != caller.AccountingID ||
		shadow.Route != routeSlug || shadow.Outcome != "success" || len(shadow.Attempts) != 1 {
		t.Fatalf("shadow envelope %+v", shadow)
	}
	if event := accountingEvent(shadow); event == nil || event.APIKeyID != "" || event.Origin != usage.OriginShadow {
		t.Fatalf("shadow accounting %+v", event)
	}
	if h.mock.count("a") != 1 || h.mock.count("b") != 1 {
		t.Fatalf("calls a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
	// A shadow's failure never reaches the caller.
	h.mock.set("b", status(http.StatusInternalServerError, `{}`))
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	h.gateway.WaitShadows()
	if mirrored, dropped := h.gateway.ShadowCounts(); mirrored != 2 || dropped != 0 {
		t.Fatalf("mirrored %d dropped %d", mirrored, dropped)
	}
}

func TestSelectorsNarrowTargetsByRequestFeatures(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, _ runtime.Provider) {
		route := s.Routes[routeSlug]
		route.Targets[0].Tags, route.Targets[1].Tags = []string{"fast"}, []string{"tools"}
		tools := true
		route.Selectors = []runtime.Selector{
			{ID: "tool-calls", When: runtime.Predicate{Tools: &tools}, Tags: []string{"tools"}},
			{ID: "plain", Tags: []string{"fast"}},
		}
		s.Routes[routeSlug] = route
	})
	if resp, _ := h.chat(fullKey, nil); resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	if env := h.sink.last(t); len(env.Attempts) != 1 || env.Attempts[0].Selector != "plain" || h.mock.count("a") != 1 {
		t.Fatalf("plain request attempts %+v", env.Attempts)
	}
	resp, _ := h.chat(fullKey, nil, `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`)
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	if env := h.sink.last(t); len(env.Attempts) != 1 || env.Attempts[0].Selector != "tool-calls" || h.mock.count("b") != 1 {
		t.Fatalf("tool request attempts %+v", env.Attempts)
	}
}

func TestSessionAffinityKeepsACacheKeyOnOneTarget(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, _ runtime.Provider) {
		route := s.Routes[routeSlug]
		route.Targets[1].Priority = route.Targets[0].Priority
		route.Affinity = &runtime.Affinity{Source: runtime.AffinityCacheKey}
		s.Routes[routeSlug] = route
	})
	served := map[string]bool{}
	for session := range 16 {
		key := fmt.Sprintf(`,"prompt_cache_key":"session-%d"`, session)
		var first string
		for range 3 {
			if resp, _ := h.chat(fullKey, nil, key); resp.StatusCode != http.StatusOK {
				t.Fatal(resp.Status)
			}
			provider := h.sink.last(t).Attempts[0].ProviderID
			if first == "" {
				first = provider
			} else if provider != first {
				t.Fatalf("session %d moved from %s to %s", session, first, provider)
			}
		}
		served[first] = true
	}
	if len(served) != 2 {
		t.Fatalf("sixteen sessions all landed on %v; affinity must spread sessions", served)
	}
}

func TestRoutingHeaderCannotRaisePriorityAboveTheKeyCeiling(t *testing.T) {
	h := newHarness(t, Config{})
	priority := func(class string) map[string]string {
		return map[string]string{routingHeader: `{"priority":"` + class + `"}`}
	}
	if resp, body := h.chat(fullKey, priority("high")); resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "priority_increase_forbidden" {
		t.Fatalf("a normal key raised its priority: %d %v", resp.StatusCode, body)
	}
	if resp, body := h.chat(fullKey, priority("low")); resp.StatusCode != http.StatusOK {
		t.Fatalf("a key could not lower its priority: %d %v", resp.StatusCode, body)
	}
	authority := h.rt.keys[fullKey]
	authority.Policy.MaxPriority = new("high")
	h.rt.keys[fullKey] = authority
	if resp, body := h.chat(fullKey, priority("high")); resp.StatusCode != http.StatusOK {
		t.Fatalf("a key could not use its ceiling: %d %v", resp.StatusCode, body)
	}
	if resp, body := h.chat(fullKey, priority("critical")); resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "priority_increase_forbidden" {
		t.Fatalf("a key exceeded its ceiling: %d %v", resp.StatusCode, body)
	}
}

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

func TestProbeRequestsParseForEveryProbedOperation(t *testing.T) {
	for operation, shape := range probeRequests {
		parsed, err := protocols.Parse(shape.family, fmt.Appendf(nil, shape.body, routeSlug), routeSlug)
		if err != nil {
			t.Fatalf("%s probe: %v", operation, err)
		}
		if parsed.Family.Operation() != operation || parsed.Route != routeSlug {
			t.Fatalf("%s probe parsed as %s for %q", operation, parsed.Family.Operation(), parsed.Route)
		}
	}
}

const classifierSlug = "team-classifier"

// triage tags provider a simple and provider b complex, adds a classifier
// route that b serves, and routes complex requests by the given predicate.
func triage(s *runtime.Snapshot, b runtime.Provider, when runtime.Predicate) {
	route := s.Routes[routeSlug]
	route.Targets[0].Tags, route.Targets[1].Tags = []string{"simple"}, []string{"complex"}
	route.Selectors = []runtime.Selector{
		{ID: "hard", When: when, Tags: []string{"complex"}},
		{ID: "easy", Tags: []string{"simple"}},
	}
	classifier := route
	classifier.Behavior = runtime.Behavior{}
	id := uuid.NewString()
	classifier.ID, classifier.RoutingID, classifier.RevisionID, classifier.Slug = id, id, uuid.NewString(), classifierSlug
	classifier.Targets = []runtime.Target{{ID: uuid.NewString(), ProviderID: b.ID, ProviderModel: modelB, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()}}
	s.Routes[routeSlug], s.Routes[classifierSlug] = route, classifier
}

func TestClassifierPredicateRoutesByTheLabelAGenerationRouteReturns(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		triage(s, b, runtime.Predicate{Classifier: &runtime.ClassifierPredicate{Route: classifierSlug, Labels: []string{"complex"}, TimeoutMS: 1000}})
	})
	for _, tc := range []struct {
		name, selector, served string
		classifier             http.HandlerFunc
		status                 int
	}{
		{"complex label", "hard", "b", completion(modelB, " complex\n"), http.StatusOK},
		{"other label", "easy", "a", completion(modelB, "simple"), http.StatusOK},
		{"classifier failure falls through", "easy", "a", status(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.mock.set("b", tc.classifier)
			before := len(h.sink.envs)
			if resp, body := h.chat(fullKey, nil, `,"messages":[{"role":"user","content":"prove the theorem"}]`); resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s", resp.Status, body)
			}
			caller := h.sink.last(t)
			h.sink.mu.Lock()
			envs := slices.Clone(h.sink.envs[before:])
			h.sink.mu.Unlock()
			if len(envs) != 2 {
				t.Fatalf("envelopes %d, want the classifier's and the caller's", len(envs))
			}
			classified := envs[0]
			if classified.Origin != usage.OriginClassifier || classified.ParentRequestID != caller.AccountingID || classified.KeyID != h.keyID || classified.Status != tc.status {
				t.Fatalf("classifier envelope %+v", classified)
			}
			if classified.Route != classifierSlug || len(classified.Attempts) == 0 || classified.Attempts[0].ProviderID != h.provider("b") {
				t.Fatalf("classifier attempts %+v", classified.Attempts)
			}
			if len(caller.Attempts) != 1 || caller.Attempts[0].Selector != tc.selector || caller.Attempts[0].ProviderID != h.provider(tc.served) {
				t.Fatalf("caller attempts %+v", caller.Attempts)
			}
		})
	}
}

// predicates answers plugin predicates in-process.
type predicates func(digest string, input abi.RoutePredicate) (bool, error)

func (p predicates) RoutePredicate(_ context.Context, digest string, input abi.RoutePredicate) (bool, error) {
	return p(digest, input)
}

func TestPluginPredicateNarrowsByFeaturesAndFallsThroughOnFailure(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	var seen []abi.RoutePredicate
	fail := false
	h := newHarness(t, Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096,
		Predicates: predicates(func(got string, input abi.RoutePredicate) (bool, error) {
			if got != digest {
				t.Errorf("digest %q", got)
			}
			seen = append(seen, input)
			if fail {
				return false, errors.New("trap")
			}
			return input.Tools, nil
		})})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		triage(s, b, runtime.Predicate{Plugin: &runtime.PluginPredicate{Digest: digest}})
	})
	h.mock.set("b", completion(modelB, answerText))
	tools := `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`
	for _, tc := range []struct {
		name, fields, selector, served string
		fail                           bool
	}{
		{"match", tools, "hard", "b", false},
		{"no match", "", "easy", "a", false},
		{"failure falls through", tools, "easy", "a", true},
	} {
		fail = tc.fail
		if resp, body := h.chat(fullKey, nil, tc.fields); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %s %s", tc.name, resp.Status, body)
		}
		if attempts := h.sink.last(t).Attempts; len(attempts) != 1 || attempts[0].Selector != tc.selector || attempts[0].ProviderID != h.provider(tc.served) {
			t.Fatalf("%s: attempts %+v", tc.name, attempts)
		}
	}
	if len(seen) != 3 || seen[0].Route != routeSlug || seen[0].Selector != "hard" || seen[0].Operation != "generation" || !seen[0].Tools || seen[1].Tools || seen[0].InputTokens <= 0 {
		t.Fatalf("predicate inputs %+v", seen)
	}
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
	h.republish(func(s *runtime.Snapshot, a, b runtime.Provider) {
		for _, provider := range []runtime.Provider{a, b} {
			provider.Limits = &runtime.Limits{MaxConcurrency: int64ptr(8)}
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
