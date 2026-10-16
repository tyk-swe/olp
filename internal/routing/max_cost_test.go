package routing

import (
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

func pricedInputs(ids []string, model string) *usage.RoutingInputs {
	prices := []usage.RoutingPrice{}
	for _, id := range ids {
		prices = append(prices, usage.RoutingPrice{Price: usage.Price{
			ProviderKind: "openai", ProviderID: &id, Model: model, Operation: "generation",
			InputPerMillion: ptr("1"), OutputPerMillion: ptr("10"), Currency: "USD",
		}})
	}
	return &usage.RoutingInputs{Prices: prices, RefreshedAt: time.Now()}
}

// Without a ceiling the callback never runs.

// A bound above the ceiling is excluded for the named reason.

// The narrowest scope wins: a tighter key ceiling beats a wider header.

// A candidate with no bound is ineligible for the same reason.

// A committed provider has zero marginal cost, so it stays eligible under max_cost.
