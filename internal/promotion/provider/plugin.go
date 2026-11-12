package provider

import (
	"context"
	"errors"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers/connection"
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

func (p planning) prepare(ctx context.Context, q access.Queryer, entry Entry) (connection.Configuration, []promotion.Item, error) {
	prepared, err := connection.Prepare(ctx, p.deps.Drafts.ConnectionPolicy(q), entry.Configuration)
	if code, refused := Refusal(err); refused {
		return entry.Configuration, []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindPlugin, entry.Configuration.ProfileRevision, code)}, nil
	}
	if err != nil {
		return connection.Configuration{}, nil, err
	}
	return prepared.Document(), nil, nil
}
