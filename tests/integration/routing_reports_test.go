//go:build integration

package integration_test

import (
	"math/big"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

// repDecimal reads a report's exact decimal.
func repDecimal(t *testing.T, value any) *big.Rat {
	t.Helper()
	text, _ := value.(string)
	rat, ok := new(big.Rat).SetString(text)
	if !ok {
		t.Fatalf("decimal %v", value)
	}
	return rat
}

func repSame(t *testing.T, label string, got any, want string) {
	t.Helper()
	if repDecimal(t, got).Cmp(repDecimal(t, want)) != 0 {
		t.Fatalf("%s = %v, want %s", label, got, want)
	}
}

// repSelected seeds one priced caller attempt that a selector chose,
// optionally with the cost of its usage on the target the selector avoided.
func (f *repFixture) repSelected(started time.Time, selector *string, cost string, baseline *string) {
	id := access.NewID()
	f.request(repRequest{ID: id, StartedAt: started, Route: "tiered", Operation: "generation", Surface: "openai", StatusCode: repInt(200), AttemptCount: 1})
	f.fact(repFact{RequestID: id, StartedAt: started, Ordinal: 1, ObservedAt: started.Add(time.Second), Route: "tiered",
		ProviderID: f.P1, Model: "small", Operation: "generation", Surface: "openai", Charge: "billable",
		Observed: true, Complete: true, Input: repCount(10), Output: repCount(5), Cost: &cost, Currency: repText("USD"), CountRequest: true})
	f.exec(`UPDATE olp.attempt_usage_facts SET selector = $2, baseline_cost = $3::numeric WHERE request_id = $1`, id, selector, baseline)
}

// A viewer confined to a project sees no experiment on a route outside it.
