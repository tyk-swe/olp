package quota

import (
	"testing"

	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/limits/admission"
)

func TestDemandSplitsWhatAnAttemptMayConsume(t *testing.T) {
	t.Parallel()
	demand := DemandOf(estimate.Estimate{Input: 120})
	if demand != (TokenDemand{Input: 120, Total: 120}) {
		t.Fatalf("DemandOf = %+v, want 120 input and no output", demand)
	}
	if combined := CombinedDemand(45); combined.Total != 45 || combined.Output != 0 {
		t.Fatalf("CombinedDemand = %+v, want the scalar held as input", combined)
	}
}

func TestSettledAttemptUsageChargesUncachedInputAndActualOutput(t *testing.T) {
	t.Parallel()
	cached := int64(800)
	fact := &events.AttemptFact{Usage: &wire.Usage{
		InputTokens: 1000, OutputTokens: 25, TotalTokens: 1025, CachedInputTokens: &cached,
	}}
	got := SettledAttemptUsage(fact, TokenDemand{Input: 900, Output: 4096, Total: 4996})
	want := &admission.TokenUsage{Input: 200, Output: 25, Total: 1025, Credit: true}
	if got == nil || *got != *want {
		t.Fatalf("SettledAttemptUsage = %+v, want %+v", got, want)
	}
}

func TestSettledAttemptUsageKeepsTheReservationWhenBillingIsUncertain(t *testing.T) {
	t.Parallel()
	fact := &events.AttemptFact{BillingUncertain: true, Usage: &wire.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}
	got := SettledAttemptUsage(fact, TokenDemand{Input: 900, Output: 100, Total: 1000})
	if got == nil || got.Credit || got.Total != 1000 {
		t.Fatalf("SettledAttemptUsage = %+v, want no credit and the reserved total held", got)
	}
	if settled := SettledAttemptUsage(&events.AttemptFact{}, TokenDemand{Input: 1}); settled != nil {
		t.Fatalf("SettledAttemptUsage without usage = %+v, want nil", settled)
	}
}
