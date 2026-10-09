package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/runtime"
)

type changedFidelityRuntime struct {
	*fakeRuntime
	denied string
	checks atomic.Int64
}

func (r *changedFidelityRuntime) CheckRouteFidelity(_ context.Context, route runtime.Route) error {
	r.checks.Add(1)
	if route.Slug == r.denied {
		return errors.New("the installed strict contract is no longer published")
	}
	return nil
}

func TestStrictAdmissionConfirmsPublicationBeforeProviderWork(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness func(*testing.T) *harness
		path    string
		body    string
	}{
		{"generation", func(t *testing.T) *harness { return strictHarness(t, nil) }, "/v1/chat/completions", `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"private input"}]}`},
		{"native unary", func(t *testing.T) *harness { return strictHarness(t, strictEmbeddings) }, "/v1/embeddings", `{"model":"` + routeSlug + `","input":"private input"}`},
		{"media", func(t *testing.T) *harness { return strictMediaHarness(t, media.OpImageGeneration, nil, nil) }, "/v1/images/generations", `{"model":"` + routeSlug + `","prompt":"private input"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.harness(t)
			current := &changedFidelityRuntime{fakeRuntime: h.rt, denied: routeSlug}
			h.gateway.Runtime = current
			response := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(tc.body), nil)
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "route_fidelity_unavailable") || current.checks.Load() != 1 || h.mock.count("a")+h.mock.count("b") != 0 {
				t.Fatalf("strict admission escaped its publication: %d %s checks=%d", response.StatusCode, body, current.checks.Load())
			}
		})
	}
}

func TestStrictPublicationCheckFollowsRouteDelegationAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		h := strictHarness(t, strictEmbeddings)
		behavior := runtime.Behavior{Selectors: []runtime.Selector{{ID: "delegate", Route: backupSlug}}}
		if fallback {
			behavior = runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: []string{runtime.FallbackExhausted}}}}
		}
		h.republish(func(snapshot *runtime.Snapshot, _, b runtime.Provider) { splitRoutes(snapshot, b, behavior) })
		if fallback {
			h.gateway.health.cooldown(h.provider("a"), h.slotA, time.Minute)
		}
		current := &changedFidelityRuntime{fakeRuntime: h.rt, denied: backupSlug}
		h.gateway.Runtime = current
		response := h.do(t.Context(), http.MethodPost, "/v1/embeddings", fullKey, []byte(`{"model":"`+routeSlug+`","input":"private input"}`), nil)
		response.Body.Close()
		if response.StatusCode < 400 || current.checks.Load() < 2 || h.mock.count("a")+h.mock.count("b") != 0 {
			t.Fatalf("replanning bypassed strict publication: fallback=%t status=%d checks=%d", fallback, response.StatusCode, current.checks.Load())
		}
	}
}

func TestPublicationCheckPreservesValidStrictAndTransformedRequests(t *testing.T) {
	for _, strict := range []bool{false, true} {
		var h *harness
		if strict {
			h = strictHarness(t, nil)
		} else {
			h = newHarness(t, Config{})
		}
		current := &changedFidelityRuntime{fakeRuntime: h.rt, denied: "another-route"}
		h.gateway.Runtime = current
		response, body := h.chat(fullKey, nil)
		wantChecks := int64(0)
		if strict {
			wantChecks = 1
		}
		if response.StatusCode != http.StatusOK || current.checks.Load() != wantChecks || h.mock.count("a") != 1 {
			t.Fatalf("valid request refused: strict=%t status=%d body=%v checks=%d", strict, response.StatusCode, body, current.checks.Load())
		}
	}
}
