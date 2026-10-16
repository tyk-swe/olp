package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
)

type changedFidelityRuntime struct {
	*fakeRuntime
	denied string
	checks atomic.Int64
}

func (r *changedFidelityRuntime) CheckRouteFidelity(_ context.Context, route routing.Route) error {
	r.checks.Add(1)
	if route.Slug == r.denied {
		return errors.New("the installed strict contract is no longer published")
	}
	return nil
}

func TestStrictPublicationCheckFollowsRouteDelegationAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		h := strictHarness(t, strictEmbeddings)
		behavior := routing.Behavior{Selectors: []routing.Selector{{ID: "delegate", Route: backupSlug}}}
		if fallback {
			behavior = routing.Behavior{Fallbacks: []routing.Fallback{{Route: backupSlug, On: []string{routing.FallbackExhausted}}}}
		}
		h.republish(func(snapshot *routing.Snapshot, _, b routing.Provider) { splitRoutes(snapshot, b, behavior) })
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
