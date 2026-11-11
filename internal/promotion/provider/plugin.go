package provider

import (
	"context"
	"errors"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers"
)

func Refusal(err error) (string, bool) {
	problem, ok := errors.AsType[*access.Problem](err)
	if !ok {
		return "", false
	}
	switch problem.Code {
	case plugins.CodeNotInstalled, plugins.CodeNotApproved, plugins.CodeUnconfinedDisabled:
		return problem.Code, true
	}
	return "", false
}

func validated(configuration providers.Configuration, policy *egress.Policy) (providers.Configuration, error) {
	if configuration.Kind == connectors.KindPlugin {
		return configuration, nil
	}
	normalized := configuration
	if err := normalized.ValidateAndNormalize(policy); err != nil {
		return providers.Configuration{}, err
	}
	return normalized, nil
}

func (p planning) prepare(ctx context.Context, q access.Queryer, entry Entry) (providers.Configuration, []promotion.Item, error) {
	prepared, err := p.deps.Catalogue.PrepareConfiguration(ctx, q, entry.Configuration)
	if code, refused := Refusal(err); refused {
		return entry.Configuration, []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindPlugin, entry.Configuration.ProfileRevision, code)}, nil
	}
	if err != nil {
		return providers.Configuration{}, nil, err
	}
	return prepared.Configuration(), nil, nil
}
