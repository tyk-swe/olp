package gateway

import (
	"context"
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
