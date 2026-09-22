package gateway

import (
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/runtime"
)

type preparedProvider struct {
	invocation providerinvoke.Invocation
	estimate   int64
}

// Prepared invocations are retained only for this inference request and bounded
// by its published route targets. Admission, estimation and dispatch consume the
// same effective defaults; accounting is still owned by the existing Attempt.
func (x *execution) preparedProvider(provider *runtime.Provider, model string) (preparedProvider, error) {
	key := provider.ID + "/" + provider.RevisionID + "/" + model
	if prepared, ok := x.preparedProviders[key]; ok {
		return prepared, nil
	}
	invocation, err := providerinvoke.Prepare(x.parsed, provider.Connector(), model, provider.ParameterDefaults)
	if err != nil {
		return preparedProvider{}, err
	}
	native := openai.NewSourceEnvelope(invocation.Wire, x.parsed.Route, x.parsed.Stream, invocation.Prepared.Document())
	estimate := max(estimateTokens(x.parsed), estimateTokens(native))
	// Bedrock's native tool catalogue is outside the OpenAI tools field. Its
	// whole schema still contributes to the same conservative token reservation.
	if invocation.Wire == openai.FamilyBedrock {
		estimate = addBounded(estimate, estimateSchema(native.Field("toolConfig")))
	}
	prepared := preparedProvider{invocation: invocation, estimate: estimate}
	if x.preparedProviders == nil {
		x.preparedProviders = map[string]preparedProvider{}
	}
	x.preparedProviders[key] = prepared
	return prepared, nil
}

func (x *execution) providerEstimate(provider *runtime.Provider) int64 {
	if provider.ProfileID == "" {
		return estimateTokens(x.parsed, provider.ParameterDefaults)
	}
	var estimate int64
	for _, attempt := range x.attempts {
		if attempt.ProviderID != provider.ID {
			continue
		}
		prepared, err := x.preparedProvider(provider, attempt.UpstreamModel)
		if err != nil {
			return limits.MaxCounter
		}
		estimate = max(estimate, prepared.estimate)
	}
	return max(estimate, 1)
}
