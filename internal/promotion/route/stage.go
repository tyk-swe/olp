package route

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
)

func Planner(desired Desired, names promotion.Names) promotion.Planner {
	return promotion.Planner{Plan: func(ctx context.Context, q database.Queryer) ([]promotion.Item, error) {
		state, err := Read(ctx, q)
		if err != nil {
			return nil, err
		}
		if err := checkGroups(ctx, q, state.directory, desired.groups); err != nil {
			return nil, err
		}
		known := func(name string) bool {
			_, ok := state.directory.ID(name)
			return ok || names.Project(name)
		}
		routeItems, err := promotion.TryMap(desired.Routes, func(r Route) ([]promotion.Item, error) { return planRoute(r, state, known) })
		if err != nil {
			return nil, err
		}
		templateItems, err := planTemplates(ctx, q, desired.Templates, state, known)
		if err != nil {
			return nil, err
		}
		return slices.Concat(slices.Concat(routeItems...), slices.Concat(templateItems...)), nil
	}}
}

func planRoute(r Route, state State, known func(string) bool) ([]promotion.Item, error) {
	unchanged, err := state.unchanged(r)
	if err != nil {
		return nil, err
	}
	return slices.Concat(unknownProject(r, known), lifecycle(r, state), []promotion.Item{stageItem(r.Slug, unchanged)}), nil
}

func stageItem(slug string, unchanged bool) promotion.Item {
	if unchanged {
		return promotion.Item{Kind: contract.ConfigurationPlanItemKindRoute, Key: slug, Action: contract.ConfigurationPlanItemActionNoop}
	}
	return promotion.Item{Kind: contract.ConfigurationPlanItemKindRoute, Key: slug, Action: contract.ConfigurationPlanItemActionStage, Detail: "route_draft"}
}

func unknownProject(r Route, known func(string) bool) []promotion.Item {
	if r.Project == nil || known(*r.Project) {
		return nil
	}
	return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindRoute, r.Slug, "project_unknown")}
}

func lifecycle(r Route, state State) []promotion.Item {
	existing, ok := state.published[r.Slug]
	switch {
	case !ok:
		return nil
	case !strings.EqualFold(promotion.ValueOf(existing.Project), promotion.ValueOf(r.Project)):
		return []promotion.Item{promotion.Conflict(contract.ConfigurationPlanItemKindRoute, r.Slug, "route_project_mismatch")}
	case r.Retired && !existing.Retired:
		return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindRoute, r.Slug, "route_lifecycle_requires_retirement")}
	case !r.Retired && existing.Retired:
		return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindRoute, r.Slug, "route_lifecycle_requires_activation")}
	}
	return nil
}

func Applier(desired Desired) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p authz.Principal) error {
		state, err := Read(ctx, tx)
		if err != nil {
			return err
		}
		names, err := providers.ReadProviderNames(ctx, tx)
		if err != nil {
			return err
		}
		installed := promotion.DirectoryOf(names)
		return promotion.Every(desired.Routes, func(r Route) error { return stage(ctx, tx, p, r, state, installed) })
	}
}

func stage(ctx context.Context, tx pgx.Tx, p authz.Principal, r Route, state State, installed promotion.Directory) error {
	unchanged, err := state.unchanged(r)
	if err != nil || unchanged {
		return err
	}
	projectID, err := projectIDOf(state.directory, r.Project)
	if err != nil {
		return err
	}
	targetInputs, err := promotion.TryMap(r.Targets, func(t Target) (routes.TargetInput, error) {
		providerID, ok := installed.ID(t.Provider)
		if !ok {
			return routes.TargetInput{}, promotion.UnknownProvider(t.Provider)
		}
		model := t.ProviderModel
		return routes.TargetInput{ProviderID: &providerID, ProviderModel: &model, Priority: t.Priority, Weight: t.Weight, TimeoutMS: t.TimeoutMS, Tags: t.Tags, Shadow: t.Shadow}, nil
	})
	if err != nil {
		return err
	}
	in := routes.DraftInput{
		Behavior: r.Behavior, Slug: r.Slug, Operations: r.Operations, OverallTimeoutMS: r.OverallTimeoutMS, MaxAttempts: r.MaxAttempts,
		Fidelity: r.Fidelity, Targets: targetInputs,
	}
	targets, err := routes.ValidateAndNormalizeDraftInput(ctx, tx, &in, projectID, nil)
	if err != nil {
		return err
	}
	slot, id := draftSlot(state.drafts, r, p, projectID)
	if err := routes.StageDraft(ctx, tx, slot, httpkit.NewID(), in.Content(targets, &r.ExposeUpstreamModels)); err != nil {
		return err
	}
	return routes.StageDraftPolicy(ctx, tx, id, httpkit.NewID(), p.UserID(), r.RoutingPolicy)
}

func draftSlot(drafts map[string]draft, r Route, p authz.Principal, projectID *string) (routes.DraftSlot, string) {
	if staged, ok := drafts[draftKey(r.Slug, r.Project)]; ok {
		return routes.ExistingDraft{ID: staged.id}, staged.id
	}
	id := httpkit.NewID()
	return routes.NewDraft{ID: id, CreatedBy: p.UserID(), ProjectID: projectID}, id
}
