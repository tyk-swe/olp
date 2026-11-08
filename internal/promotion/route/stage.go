package route

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
)

func Planner(desired Desired, names promotion.Names) promotion.Planner {
	return promotion.Planner{Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
		if err := checkGroups(ctx, q, desired.groups); err != nil {
			return nil, err
		}
		state, err := Read(ctx, q)
		if err != nil {
			return nil, err
		}
		known := func(name string) bool {
			_, ok := state.directory.ID(name)
			return ok || names.Project(name)
		}
		templates := promotion.Index(state.Templates, func(t Template) string { return t.Name })
		routeItems, err := promotion.TryMap(desired.Routes, func(r Route) ([]promotion.Item, error) { return planRoute(r, state, known) })
		if err != nil {
			return nil, err
		}
		templateItems, err := promotion.TryMap(desired.Templates, func(t Template) ([]promotion.Item, error) { return planTemplate(t, templates, known) })
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
	case !strings.EqualFold(deref(existing.Project), deref(r.Project)):
		return []promotion.Item{promotion.Conflict(contract.ConfigurationPlanItemKindRoute, r.Slug, "route_project_mismatch")}
	case r.Retired && !existing.Retired:
		return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindRoute, r.Slug, "route_lifecycle_requires_retirement")}
	case !r.Retired && existing.Retired:
		return []promotion.Item{promotion.Blocker(contract.ConfigurationPlanItemKindRoute, r.Slug, "route_lifecycle_requires_activation")}
	}
	return nil
}

func Applier(desired Desired) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		state, err := Read(ctx, tx)
		if err != nil {
			return err
		}
		providerIDs, err := providers.IDsByName(ctx, tx)
		if err != nil {
			return err
		}
		return promotion.Every(desired.Routes, func(r Route) error { return stage(ctx, tx, p, r, state, providerIDs) })
	}
}

func stage(ctx context.Context, tx pgx.Tx, p access.Principal, r Route, state State, providerIDs map[string]string) error {
	unchanged, err := state.unchanged(r)
	if err != nil || unchanged {
		return err
	}
	projectID, err := projectIDOf(state.directory, r.Project)
	if err != nil {
		return err
	}
	in := routes.DraftInput{
		Behavior: r.Behavior, Slug: r.Slug, Operations: r.Operations, OverallTimeoutMS: r.OverallTimeoutMS, MaxAttempts: r.MaxAttempts,
		ContentPolicy: r.ContentPolicy, Fidelity: r.Fidelity,
		Targets: promotion.Map(r.Targets, func(t Target) routes.TargetInput {
			providerID, model := providerIDs[strings.ToLower(t.Provider)], t.ProviderModel
			return routes.TargetInput{ProviderID: &providerID, ProviderModel: &model, Priority: t.Priority, Weight: t.Weight, TimeoutMS: t.TimeoutMS, Tags: t.Tags, Shadow: t.Shadow}
		}),
	}
	targets, err := routes.ValidateAndNormalizeDraftInput(ctx, tx, &in, projectID, nil)
	if err != nil {
		return err
	}
	slot, id := draftSlot(state.drafts, r, p, projectID)
	if err := routes.StageDraft(ctx, tx, slot, access.NewID(), in.Content(targets, &r.ExposeUpstreamModels)); err != nil {
		return err
	}
	return routes.StageDraftPolicy(ctx, tx, id, access.NewID(), p.UserID(), r.RoutingPolicy)
}

func draftSlot(drafts map[string]draft, r Route, p access.Principal, projectID *string) (routes.DraftSlot, string) {
	if staged, ok := drafts[draftKey(r.Slug, r.Project)]; ok {
		return routes.ExistingDraft{ID: staged.id}, staged.id
	}
	id := access.NewID()
	return routes.NewDraft{ID: id, CreatedBy: p.UserID(), ProjectID: projectID}, id
}
