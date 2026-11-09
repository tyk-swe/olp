package retrievalbinding

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/retrieval"
)

const (
	kind         = contract.ConfigurationPlanItemKindRetrievalBinding
	stagedDetail = "Bind the staged destination route target and explicit project-local key grants; queries require a published certified target."
	storedDetail = "Retain immutable revision history, explicit key grants and the configured backend resource; serving still requires a published certified target."
)

func Planner(desired Desired) promotion.Planner {
	if len(desired.Bindings) == 0 {
		return promotion.Planner{}
	}
	return promotion.Planner{Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
		lookup, err := readInstalled(ctx, q)
		if err != nil {
			return nil, err
		}
		return promotion.TryMap(desired.Bindings, func(binding Binding) (promotion.Item, error) {
			return planBinding(ctx, q, lookup, desired.stages[key(binding)], binding)
		})
	}}
}

func planBinding(ctx context.Context, q access.Queryer, lookup installed, routeStage stage, binding Binding) (promotion.Item, error) {
	switch routeStage := routeStage.(type) {
	case misstaged:
		return promotion.Blocker(kind, binding.Name, routeStage.detail), nil
	case staged:
		return planStaged(ctx, q, lookup.projects, routeStage, binding)
	default:
		return planStored(ctx, q, lookup, binding)
	}
}

func planStaged(ctx context.Context, q access.Queryer, directory promotion.Directory, routeStage staged, binding Binding) (promotion.Item, error) {
	projectID, exists := directory.ID(binding.Project)
	if !exists && (!routeStage.projectDeclared || len(binding.KeyGrants) > 0) {
		return promotion.Blocker(kind, binding.Name, "Create the destination project and named API-key grants before promoting access."), nil
	}
	for _, name := range binding.KeyGrants {
		ids, err := usableKeys(ctx, q, projectID, name)
		if err != nil {
			return promotion.Item{}, err
		}
		if len(ids) != 1 {
			return promotion.Blocker(kind, binding.Name, "Each grant must resolve to exactly one active destination API key by its project-local name."), nil
		}
	}
	return promotion.Item{Kind: kind, Key: binding.Name, Action: contract.ConfigurationPlanItemActionStage, Detail: stagedDetail}, nil
}

func planStored(ctx context.Context, q access.Queryer, lookup installed, binding Binding) (promotion.Item, error) {
	in, err := resolve(ctx, q, lookup, binding)
	if errors.Is(err, errProjectAbsent) {
		return promotion.Blocker(kind, binding.Name, "Create the destination project and named API-key grants before applying this binding."), nil
	}
	if problem, ok := errors.AsType[*access.Problem](err); ok {
		return promotion.Blocker(kind, binding.Name, problem.Detail), nil
	}
	if err != nil {
		return promotion.Item{}, err
	}
	current, err := find(ctx, q, in)
	if err != nil {
		return promotion.Item{}, err
	}
	return promotion.Item{Kind: kind, Key: binding.Name, Action: storedAction(current, in), Detail: storedDetail}, nil
}

func storedAction(current *retrieval.Binding, in retrieval.Input) contract.ConfigurationPlanItemAction {
	if current == nil {
		return contract.ConfigurationPlanItemActionCreate
	}
	if reflect.DeepEqual(current.Input, in) {
		return contract.ConfigurationPlanItemActionReuse
	}
	return contract.ConfigurationPlanItemActionReplace
}

func find(ctx context.Context, q access.Queryer, in retrieval.Input) (*retrieval.Binding, error) {
	current, err := retrieval.Find(ctx, q, in.ProjectID, in.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return current, err
}
