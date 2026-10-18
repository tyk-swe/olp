package gateway

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"
)

func TestCommitmentPinDoesNotInventPricingPin(t *testing.T) {
	event := Envelope{Operation: "generation", Attempts: []AttemptFact{{StartedAt: time.Now(), CommitmentPinned: true, Commitment: &routing.Commitment{ID: "schedule", Amount: "10", Period: "day"}}}}
	attempt := accountingAttempt(event, 0)
	var policy struct {
		CommitmentPinned bool `json:"commitment_pinned"`
		PricingPinned    bool `json:"pricing_pinned"`
		DiscountPinned   bool `json:"discount_pinned"`
	}
	if attempt.Routing.Policy == nil {
		t.Fatal("commitment provenance missing")
	}
	if err := json.Unmarshal(*attempt.Routing.Policy, &policy); err != nil {
		t.Fatal(err)
	}
	if !policy.CommitmentPinned || policy.PricingPinned || policy.DiscountPinned {
		t.Fatalf("independent pins: %+v", policy)
	}
}
