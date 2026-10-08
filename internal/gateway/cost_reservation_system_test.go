//go:build integration

package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func installBudgetBalance(t *testing.T, limiter *limits.Limiter, owner string) {
	t.Helper()
	windows := limits.BudgetWindows(time.Now())
	_, _, err := limiter.ApplyCostSnapshot(t.Context(), limits.CostSnapshot{
		CostOwnerID: owner, DailyWindowID: windows.DailyID, MonthlyWindowID: windows.MonthlyID,
		DailyAccrued: "0", MonthlyAccrued: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationFallbackReservesCallerBudgetsBeforeDispatch(t *testing.T) {
	for _, owner := range []string{"key", "group", "both"} {
		for _, firstPriced := range []bool{true, false} {
			for _, affordable := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/priced=%t/affordable=%t", owner, firstPriced, affordable), func(t *testing.T) {
					h := newHarness(t, Config{})
					h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
						splitRoutes(s, b, runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: []string{runtime.FallbackExhausted}}}})
					})
					price := usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai_compatible", Model: modelB, Operation: "generation",
						InputPerMillion: costText("10"), OutputPerMillion: costText("100"),
					}}
					h.rt.inputs = &usage.RoutingInputs{RefreshedAt: time.Now(), Prices: []usage.RoutingPrice{price}}
					if firstPriced {
						price.Model = modelA
						price.InputPerMillion, price.OutputPerMillion = costText("1"), costText("10")
						h.rt.inputs.Prices = append(h.rt.inputs.Prices, price)
					}
					limiter := mediaLimiter(t)
					h.gateway.Admission = NewAdmission(limiter, nil, h.gateway.log)
					authority := h.rt.keys[fullKey]
					authority.LookupID = strings.ReplaceAll(uuid.NewString(), "-", "")
					ceiling := "0.001"
					if firstPriced {
						// Either leg fits alone; together they exceed the budget.
						ceiling = "0.0017"
					}
					if affordable {
						ceiling = "0.01"
					}
					if owner != "group" {
						authority.Policy.DailyCostLimit = &ceiling
						installBudgetBalance(t, limiter, authority.ID)
					}
					if owner != "key" {
						group := uuid.NewString()
						authority.BudgetGroupID, authority.BudgetGroupDailyCostLimit = &group, &ceiling
						installBudgetBalance(t, limiter, group)
					}
					one := int64(1)
					authority.Policy.RequestsPerMinute = &one
					h.rt.keys[fullKey] = authority
					h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
					h.mock.set("b", completion(modelB, answerText))
					resp, body := h.chat(fullKey, nil, `,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]`)
					want, fallbackCalls := http.StatusTooManyRequests, 0
					if affordable {
						want, fallbackCalls = http.StatusOK, 1
					}
					if resp.StatusCode != want || h.mock.count("a") != 1 || h.mock.count("b") != fallbackCalls {
						t.Fatalf("status=%d want=%d calls=%d/%d body=%s", resp.StatusCode, want, h.mock.count("a"), h.mock.count("b"), body)
					}
				})
			}
		}
	}
}

func TestIntegrationRealtimeSessionsHoldTheirTargetsPrice(t *testing.T) {
	// A session admits before its price is known, so its target's price is held
	// before the provider is dialled: 100 tokens at 10000 per million is 1.
	for _, tc := range []struct {
		ceiling string
		refused bool
	}{{"5", false}, {"0.5", true}} {
		t.Run(tc.ceiling, func(t *testing.T) {
			h := newHarness(t, Config{})
			limiter := mediaLimiter(t)
			h.gateway.Admission = NewAdmission(limiter, nil, h.gateway.log)
			authority := h.rt.keys[fullKey]
			authority.LookupID = strings.ReplaceAll(uuid.NewString(), "-", "")
			authority.Policy.DailyCostLimit = &tc.ceiling
			installBudgetBalance(t, limiter, authority.ID)
			x := &execution{request: request{id: uuid.NewString(), minted: true, release: h.rt.release}, estimate: resourceEstimate}
			lease, e := h.gateway.Admission.reserveKey(t.Context(), authority, "openai", resourceEstimate, time.Minute)
			if e != nil {
				t.Fatal(e)
			}
			x.lease = lease
			t.Cleanup(func() { settleKey(t.Context(), lease, false, nil, h.gateway.log) })
			attempt := runtime.Attempt{Price: &usage.RoutingPrice{Price: usage.Price{InputPerMillion: costText("1000"), OutputPerMillion: costText("1000")}}}
			e = h.gateway.reserveSessionCost(t.Context(), x, authority, attempt, time.Minute)
			if (e != nil) != tc.refused || e != nil && e.Status != http.StatusTooManyRequests {
				t.Fatalf("session hold: %v, want refused=%t", e, tc.refused)
			}
			if !tc.refused && !lease.HasCostReservation() {
				t.Fatal("the session's price was not held")
			}
		})
	}
}
