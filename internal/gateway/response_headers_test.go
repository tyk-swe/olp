package gateway

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

// The literal forms the OpenAI documentation shows.

// rateAllowance is a key's allowance a quarter of the way through a window that
// ends at a known instant.
func rateAllowance(requests, tokens bool) limits.RateState {
	state := limits.RateState{ResetAfter: 37500 * time.Millisecond, ResetAt: time.Date(2026, 10, 2, 9, 31, 0, 0, time.UTC)}
	if requests {
		state.RequestLimit, state.RequestRemaining = 60, 59
	}
	if tokens {
		state.TokenLimit, state.TokenRemaining = 150000, 149984
	}
	return state
}

// Gemini and Bedrock SDKs read no such headers, and nothing reads a native
// operation's: none are sent.

// A key that limits neither dimension has no allowance to state.

// Quotas of a provider and of a cost budget are not the caller's allowance.

// reservedLease is the reservation of a key limited per minute as the limiter
// answers it, with the allowance and the reset a response states.
func reservedLease(t *testing.T, requests, tokens int64) *limits.Lease {
	t.Helper()
	limiter, err := limits.New(&window{}, "olp:test")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := limiter.Reserve(t.Context(), limits.Request{
		CostOwnerID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607", LookupID: "lookup_allocations", RequestsPerMinute: &requests, TokensPerMinute: &tokens,
		RequestedTokens: headerFixtureTokens, LeaseTTL: 5 * time.Second, ReportRate: true,
	})
	if err != nil || !lease.RateState().Limited() {
		t.Fatalf("Reserve: %+v, %v", lease.RateState(), err)
	}
	return lease
}

// headers written

// The Anthropic SDKs report an error's request id from this header.

func decimalText(value string) *string { return &value }

func billedFact(price *usage.RoutingPrice, input, output int64) AttemptFact {
	return AttemptFact{
		Class: classSuccess, Price: price, UsageObserved: true, UsageComplete: true,
		Usage: &wire.Usage{InputTokens: input, OutputTokens: output, TotalTokens: input + output},
	}
}

// failedAfterDispatch is an attempt the upstream may have billed, without saying.
func failedAfterDispatch(price *usage.RoutingPrice) AttemptFact {
	return AttemptFact{Class: classUpstreamServer, Price: price, BillingUncertain: true}
}

// A million tokens of input cost one unit and of output ten.

// A call that was never billable, as a lifecycle read of a stored response
// is not, has no cost to state, which accounting records as no cost at all.
