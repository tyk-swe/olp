package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func callerHarness(t *testing.T) *harness {
	t.Helper()
	h := strictHarness(t, nil)
	h.republish(func(s *runtime.Snapshot, _, _ runtime.Provider) {
		for id, p := range s.Providers {
			p.CredentialSource = "caller"
			s.Providers[id] = p
		}
	})
	return h
}
func TestCallerCredentialIsRequestLocalAndRecordedWithoutSecret(t *testing.T) {
	h := callerHarness(t)
	const secret = "caller-secret-private"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get(callerCredentialHeader) != "" {
			t.Error("caller authentication or local-header removal failed")
		}
		completion(modelA, answerText)(w, r)
	})
	response, body := h.chat(fullKey, map[string]string{callerCredentialHeader: secret})
	if response.StatusCode != 200 {
		t.Fatalf("status=%d body=%v", response.StatusCode, body)
	}
	e := accountingEvent(h.sink.last(t))
	raw, err := usage.Encode(e)
	if err != nil || bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("private accounting: %v", err)
	}
	decoded, err := usage.Decode(raw)
	if err != nil || decoded.Attempts[0].Routing.CredentialSource != "caller" || decoded.Attempts[0].Routing.CredentialVersionID != nil {
		t.Fatalf("source metadata missing: %v", err)
	}
}
func TestCallerCredentialRefusalDoesNotPoisonProbeOrCrossTargets(t *testing.T) {
	h := callerHarness(t)
	const secret = "rejected-caller-secret"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "rejected " + secret, "type": "authentication_error"}})
	})
	response, body := h.chat(fullKey, map[string]string{callerCredentialHeader: secret})
	encoded, _ := json.Marshal(body)
	if response.StatusCode < 400 || bytes.Contains(encoded, []byte(secret)) || h.mock.count("b") != 0 {
		t.Fatalf("refusal leaked, succeeded, or crossed targets: %d %s", response.StatusCode, encoded)
	}
	h.mock.set("a", completion(modelA, answerText))
	response, _ = h.chat(fullKey, map[string]string{callerCredentialHeader: "valid-new-caller"})
	if response.StatusCode != 200 {
		t.Fatalf("caller failure poisoned operator probe or pool: %d", response.StatusCode)
	}
}
func TestCallerCredentialsMissingDuplicateAndOversizedFailBeforeDispatch(t *testing.T) {
	for _, values := range [][]string{nil, {"one", "two"}, {strings.Repeat("x", 16385)}} {
		t.Run(strings.Join([]string{"values", string(rune('0' + len(values)))}, "-"), func(t *testing.T) {
			h := callerHarness(t)
			r, _ := http.NewRequest(http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+routeSlug+`","messages":[{"role":"user","content":"hello"}]}`))
			r.Header.Set("Authorization", "Bearer "+fullKey)
			r.Header.Set("Content-Type", "application/json")
			for _, v := range values {
				r.Header.Add(callerCredentialHeader, v)
			}
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != 400 || h.mock.count("a")+h.mock.count("b") != 0 {
				t.Fatalf("unsafe request dispatched: %d", resp.StatusCode)
			}
		})
	}
}

func TestCallerCostExemptionRequiresCallerTargetsAndMatchingRouteTransitions(t *testing.T) {
	h := callerHarness(t)
	snapshot := h.rt.Release().Snapshot
	route := snapshot.Routes[routeSlug]
	route.CallerCostExempt = true
	snapshot.Routes[routeSlug] = route
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	providerID := route.Targets[0].ProviderID
	provider := snapshot.Providers[providerID]
	provider.CredentialSource = "operator"
	snapshot.Providers[providerID] = provider
	if err := snapshot.Validate(); err == nil {
		t.Fatal("operator target became exempt")
	}
	provider.CredentialSource = "caller"
	snapshot.Providers[providerID] = provider
	backup := route
	backup.Slug = "backup"
	backup.CallerCostExempt = false
	snapshot.Routes[backup.Slug] = backup
	route.Fallbacks = []runtime.Fallback{{Route: backup.Slug, On: []string{runtime.FallbackExhausted}}}
	snapshot.Routes[routeSlug] = route
	if err := snapshot.Validate(); err == nil {
		t.Fatal("route transition bypassed cost admission")
	}
	route.Fallbacks = nil
	route.CallerCostExempt = false
	snapshot.Routes[routeSlug] = route
	classified := backup
	classified.Slug = "classified"
	classified.Selectors = []runtime.Selector{{When: runtime.Predicate{Classifier: &runtime.ClassifierPredicate{Route: routeSlug, Labels: []string{"code"}, TimeoutMS: 1000}}, Route: backup.Slug}}
	snapshot.Routes[classified.Slug] = classified
	if err := snapshot.Validate(); err == nil || !strings.Contains(err.Error(), "requires caller credentials") {
		t.Fatalf("classifier route requires a caller credential it never receives: %v", err)
	}
}

func TestCallerCredentialIsRemovedBeforeSemanticOrRetainedProcessing(t *testing.T) {
	headers := http.Header{}
	headers.Set(callerCredentialHeader, "private-caller-value")
	headers.Set(endUserHeader, "private-user")
	headers.Set("X-OLP-Attribution", `{"team":"core"}`)
	headers.Set("Anthropic-Version", "2023-06-01")
	semantic := semanticHeaders(headers)
	for _, name := range []string{callerCredentialHeader, endUserHeader, "X-OLP-Attribution"} {
		if semantic.Get(name) != "" {
			t.Fatalf("private header retained: %s", name)
		}
		if headers.Get(name) == "" {
			t.Fatal("mutated original request headers")
		}
	}
	if semantic.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatal("lost provider semantic header")
	}
}

// A video list polls its jobs concurrently; the credential still serves only
// the first target that binds it.
func TestCallerCredentialBindsOneTargetUnderConcurrency(t *testing.T) {
	c := &callerCredential{secret: []byte("caller-secret")}
	var bound atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			if c.bind(fmt.Sprint("provider\x00model-", i%4)) {
				bound.Add(1)
			}
		})
	}
	wg.Wait()
	if winners := bound.Load(); winners != 4 {
		t.Fatalf("bound %d calls, want the 4 that named the first target", winners)
	}
	if !c.boundElsewhere("provider\x00other") || c.boundElsewhere(c.target) {
		t.Fatal("the bound target is not the only one the credential serves")
	}
}
