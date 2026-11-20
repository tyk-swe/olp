package quota

import (
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/limits/admission"
)

type TokenDemand struct {
	Input  int64
	Output int64
	Total  int64
}

func DemandOf(e estimate.Estimate) TokenDemand {
	return TokenDemand{Input: e.InputExposure(), Output: e.Reply(), Total: e.Tokens()}
}

func CombinedDemand(tokens int64) TokenDemand {
	return TokenDemand{Input: tokens, Total: tokens}
}

func SettledAttemptUsage(fact *events.AttemptFact, demand TokenDemand) *admission.TokenUsage {
	if fact == nil {
		return nil
	}
	total := TotalTokens(fact.Usage)
	if total == nil {
		return nil
	}
	usage := fact.Usage
	input := min(usage.InputTokens, maxEstimate)
	if cached := usage.CachedInputTokens; cached != nil && *cached >= 0 && *cached <= input {
		input -= *cached
	}
	settled := &admission.TokenUsage{
		Input:  input,
		Output: min(usage.OutputTokens, maxEstimate),
		Total:  *total,
		Credit: !fact.BillingUncertain,
	}
	if fact.BillingUncertain {
		settled.Total = max(settled.Total, demand.Total)
	}
	return settled
}
