package providers

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/plugins"
)

type PreparedConfiguration struct {
	configuration Configuration
}

func PrepareConfiguration(ctx context.Context, q access.Queryer, unconfined *plugins.Unconfined, policy *egress.Policy, input Configuration) (PreparedConfiguration, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return PreparedConfiguration{}, err
	}
	var configuration Configuration
	if err := json.Unmarshal(encoded, &configuration); err != nil {
		return PreparedConfiguration{}, err
	}
	configuration.ProviderID = input.ProviderID
	configuration.ProbeModels = slices.Clone(input.ProbeModels)
	configuration.Normalize()
	if err := configuration.Pin(ctx, q, unconfined); err != nil {
		return PreparedConfiguration{}, err
	}
	if err := configuration.ValidateAndNormalize(policy); err != nil {
		return PreparedConfiguration{}, err
	}
	return PreparedConfiguration{configuration: configuration}, nil
}
