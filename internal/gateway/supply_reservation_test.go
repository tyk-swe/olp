package gateway

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
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

func TestSupplyReservationsCoverEveryDispatchedAttempt(t *testing.T) {
	for _, quota := range []string{quotaRoute, quotaConnection, quotaSlot} {
		for _, tc := range []struct {
			name, total                 string
			expensive, refuse, unpriced bool
		}{
			{name: "retry", total: "0.000324"},
			{name: "expensive failover", total: "0.001782", expensive: true},
			{name: "refused retry", total: "0.000324", refuse: true},
			{name: "refused expensive failover", total: "0.001782", expensive: true, refuse: true},
			{name: "priced after unpriced", total: "0.000162", unpriced: true},
		} {
			t.Run(quota+"/"+tc.name, func(t *testing.T) {
				x := costExecution(t, `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, openai.FamilyChat, 3,
					runtime.Attempt{Price: costPrice()})
				x.request.id, x.request.minted, x.route.ID = uuid.NewString(), true, uuid.NewString()
				attempt := x.attempts[0]
				provider := x.snapshot().Providers[attempt.ProviderID]
				slot := provider.Slots[0]
				switch quota {
				case quotaRoute:
					x.route.Budget = &runtime.CostLimits{DailyCostLimit: costText("1")}
				case quotaConnection:
					provider.Limits = &runtime.Limits{Supply: runtime.Supply{DailyCostLimit: costText("1")}}
				case quotaSlot:
					slot.Supply = runtime.Supply{DailyCostLimit: costText("1")}
				}
				server := &capReservations{refuseIncrease: tc.refuse}
				limiter, err := limits.New(server, "olp:test")
				if err != nil {
					t.Fatal(err)
				}
				s := &Server{Admission: &Admission{limiter: limiter}}
				first := attempt
				if tc.unpriced {
					first.Price = nil
				}
				deadline := time.Now().Add(time.Second)
				check := s.holdCaps(t.Context(), x, first, &provider, &slot, deadline)
				if check.quota != "" || len(x.caps) != 1 {
					t.Fatalf("first hold: %+v caps=%+v", check, x.caps)
				}
				x.spendCaps(check.owners, first)
				if tc.expensive {
					attempt.Price = &usage.RoutingPrice{Price: usage.Price{InputPerMillion: costText("10"), OutputPerMillion: costText("100")}}
				}
				check = s.holdCaps(t.Context(), x, attempt, &provider, &slot, deadline)
				if len(server.requests) != 2 || server.requests[1][10] != tc.total {
					t.Fatalf("subsequent dispatch did not reserve total %s: %v", tc.total, server.requests)
				}
				if !tc.unpriced && server.requests[0][11] != server.requests[1][11] {
					t.Fatal("an increase changed the reservation's accounting identity")
				}
				if tc.refuse {
					if check.quota != quota || check.refusal == nil || x.caps[0].reserved.String() != "0.000162" {
						t.Fatalf("refused increase lost the original hold: %+v caps=%+v", check, x.caps)
					}
				} else if check.quota != "" || x.caps[0].reserved.String() != tc.total {
					t.Fatalf("increase: %+v caps=%+v", check, x.caps)
				}
			})
		}
	}
}

func TestBackgroundResponseUsageKeepsSupplyBudgetsDuringDispatch(t *testing.T) {
	h := newHarness(t, Config{})
	x := costExecution(t, `{"model":"team-chat","input":"hello","background":true,"stream":true}`, openai.FamilyResponses, 1,
		runtime.Attempt{Price: costPrice()})
	x.request = request{id: uuid.NewString(), minted: true, startedAt: time.Now(), release: h.rt.Release()}
	x.keyID, x.mode, x.route.ID = h.keyID, "streaming", uuid.NewString()
	x.route.RoutingID = x.route.ID
	x.route.Budget = &runtime.CostLimits{DailyCostLimit: costText("1")}
	provider := x.snapshot().Providers[x.attempts[0].ProviderID]
	provider.Limits = &runtime.Limits{Supply: runtime.Supply{DailyCostLimit: costText("1")}}
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
		dispatch: func(_ context.Context, _ runtime.Attempt, _ *runtime.Provider, _ runtime.Slot, fact AttemptFact) (AttemptFact, struct{}, *attemptFailure) {
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
