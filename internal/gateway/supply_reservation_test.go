package gateway

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

// capReservations records cost holds and can refuse an increase. The real
// script's atomic increase and refusal behavior is covered by integration tests.
type capReservations struct {
	requests       [][]string
	refuseIncrease bool
}

func (c *capReservations) Do(_ context.Context, args ...string) (any, error) {
	c.requests = append(c.requests, slices.Clone(args))
	if c.refuseIncrease && len(c.requests) > 1 {
		return []any{int64(1), int64(0), "daily_cost_estimate", int64(1000), int64(20_000), int64(24_000)}, nil
	}
	return []any{int64(1), int64(1), "ok", int64(0), int64(20_000), int64(24_000)}, nil
}

func TestBackgroundResponseUsageKeepsSupplyBudgetsDuringDispatch(t *testing.T) {
	h := newHarness(t, Config{})
	x := costExecution(t, `{"model":"team-chat","input":"hello","background":true,"stream":true}`, wire.Responses, 1,
		routing.Attempt{Price: costPrice()})
	x.request = request{id: uuid.NewString(), minted: true, startedAt: time.Now(), release: h.rt.Release()}
	x.keyID, x.mode, x.route.ID = h.keyID, "streaming", uuid.NewString()
	x.route.RoutingID = x.route.ID
	x.route.Budget = &routing.CostLimits{DailyCostLimit: costText("1")}
	provider := x.snapshot().Providers[x.attempts[0].ProviderID]
	provider.Limits = &routing.Limits{Supply: routing.Supply{DailyCostLimit: costText("1")}}
	provider.Slots[0].Weight, provider.Slots[0].DailyCostLimit = 1, costText("1")
	x.snapshot().Providers[provider.ID] = provider
	owners := []string{x.route.ID, provider.ID, provider.Slots[0].ID}
	limiter, err := limits.New(&capReservations{}, "olp:test")
	if err != nil {
		t.Fatal(err)
	}
	h.gateway.Admission = NewAdmission(limiter, nil, h.gateway.log)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	metadata := map[string]json.RawMessage{}
	out := runAttempts(ctx, h.gateway, x, attemptAdapter[struct{}]{
		estimate: x.attemptReservation,
		dispatch: func(_ context.Context, _ routing.Attempt, _ *routing.Provider, _ routing.Slot, fact AttemptFact) (AttemptFact, struct{}, *attemptFailure) {
			if !h.gateway.pendingResponseUsage(x, &fact, metadata) {
				t.Fatal("background dispatch did not retain pending usage")
			}
			fact.Class = classSuccess
			return fact, struct{}{}, nil
		},
	})
	if out.err != nil {
		t.Fatal(out.err)
	}
	var event usage.Event
	if err := json.Unmarshal(metadata["pending_usage"], &event); err != nil {
		t.Fatal(err)
	}
	if len(event.Attempts) != 1 || event.Attempts[0].Routing == nil || !slices.Equal(event.Attempts[0].Routing.Budgets, owners) {
		t.Fatalf("pending usage lost supply budget owners before dispatch returned: %+v", event.Attempts)
	}
}
