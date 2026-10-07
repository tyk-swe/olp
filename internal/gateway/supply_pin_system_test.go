//go:build integration

package gateway

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestIntegrationPinnedRequestsEnforceAndSettleSupplyCaps(t *testing.T) {
	for _, scope := range []string{"route", "provider", "slot"} {
		for _, outcome := range []string{"admitted", "refused", "local failure"} {
			t.Run(scope+"/"+outcome, func(t *testing.T) {
				limiter := mediaLimiter(t)
				h := newHarness(t, Config{})
				h.gateway.Admission = NewAdmission(limiter, nil, h.gateway.log)
				snapshot := h.rt.release.Snapshot
				route := snapshot.Routes[routeSlug]
				route.Targets = route.Targets[:1]
				provider := snapshot.Providers[route.Targets[0].ProviderID]
				provider.Kind, provider.ProfileID, provider.ProfileRevision = "gemini", "gemini-interactions", connectors.ProfileRevision
				provider.Endpoint = h.upstream.URL + "/v1beta"
				provider.Capabilities = []runtime.Capability{{Model: modelA, Operation: "generation", Surface: "gemini", Mode: "unary"}}
				ceiling, owner := "1", ""
				if outcome == "refused" {
					ceiling = "0.000001"
				}
				if outcome == "local failure" {
					provider.Endpoint = "http://192.0.2.1/v1beta"
				}
				switch scope {
				case "route":
					route.Budget = &runtime.CostLimits{DailyCostLimit: &ceiling}
					owner = route.ID
				case "provider":
					provider.Limits = &runtime.Limits{DailyCostLimit: &ceiling}
					owner = provider.ID
				case "slot":
					provider.Slots[0].DailyCostLimit = &ceiling
					owner = provider.Slots[0].ID
				}
				snapshot.Providers[provider.ID], snapshot.Routes[route.Slug] = provider, route
				installBudgetBalance(t, limiter, owner)
				h.rt.inputs = &usage.RoutingInputs{RefreshedAt: time.Now(), Prices: []usage.RoutingPrice{{Price: usage.Price{
					ProviderKind: "gemini", Model: modelA, Operation: "generation", InputPerMillion: costText("1"), OutputPerMillion: costText("10"),
				}}}}
				h.mock.set("v1beta", status(200, `{"id":"int_1","status":"completed","steps":[],"usage":{"total_input_tokens":3,"total_output_tokens":2}}`))
				request := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader(`{"model":"`+routeSlug+`","input":"hello","store":false}`))
				request.Header.Set("X-Goog-Api-Key", fullKey)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				h.gateway.geminiInteractionCreate(response, request)
				calls, wantCost := 0, "0"
				switch outcome {
				case "admitted":
					calls, wantCost = 1, "0.000023"
					if response.Code != http.StatusOK {
						t.Fatalf("admitted request: %d %s", response.Code, response.Body)
					}
					env := h.sink.last(t)
					if len(env.Attempts) != 1 || !slices.Contains(env.Attempts[0].Budgets, owner) {
						t.Fatalf("pinned attempt lost its budget owner: %+v", env)
					}
					event := accountingEvent(env)
					if event == nil || len(event.Attempts) != 1 || event.Attempts[0].Routing == nil || !slices.Contains(event.Attempts[0].Routing.Budgets, owner) {
						t.Fatalf("pinned accounting lost its budget owner: %+v", event)
					}
				case "refused":
					if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "supply_budget_exhausted") {
						t.Fatalf("cap admitted an unaffordable pin: %d %s", response.Code, response.Body)
					}
				case "local failure":
					if response.Code != http.StatusBadGateway {
						t.Fatalf("local dispatch failure: %d %s", response.Code, response.Body)
					}
				}
				if got := h.mock.count("v1beta"); got != calls {
					t.Fatalf("upstream calls = %d, want %d", got, calls)
				}
				if reserved, err := limiter.Reserved(t.Context(), owner); err != nil || reserved != wantCost {
					t.Fatalf("settled pin reservation = %s, %v; want %s", reserved, err, wantCost)
				}
			})
		}
	}
}
