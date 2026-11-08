package route

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
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
		templates := byKey(state.Templates, func(t Template) string { return t.Name })
		routeItems, err := each(desired.Routes, func(_ int, r Route) ([]promotion.Item, error) { return planRoute(r, state, known) })
		if err != nil {
			return nil, err
		}
		templateItems, err := each(desired.Templates, func(_ int, t Template) ([]promotion.Item, error) { return planTemplate(t, templates, known) })
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
		providers, err := providerIDs(ctx, tx)
		if err != nil {
			return err
		}
		_, err = each(desired.Routes, func(_ int, r Route) (Route, error) { return r, stage(ctx, tx, p, r, state, providers) })
		return err
	}
}

func stage(ctx context.Context, tx pgx.Tx, p access.Principal, r Route, state State, providers map[string]string) error {
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
		Targets: slices.Collect(mapped(r.Targets, func(t Target) routes.TargetInput {
			providerID, model := providers[strings.ToLower(t.Provider)], t.ProviderModel
			return routes.TargetInput{ProviderID: &providerID, ProviderModel: &model, Priority: t.Priority, Weight: t.Weight, TimeoutMS: t.TimeoutMS, Tags: t.Tags, Shadow: t.Shadow}
		})),
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

func providerIDs(ctx context.Context, q access.Queryer) (map[string]string, error) {
	rows, err := q.Query(ctx, "SELECT lower(name),id::text FROM olp.providers")
	if err != nil {
		return nil, err
	}
	type named struct{ name, id string }
	providers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (named, error) {
		var p named
		return p, row.Scan(&p.name, &p.id)
	})
	if err != nil {
		return nil, err
	}
	return maps.Collect(func(yield func(string, string) bool) {
		for _, p := range providers {
			if !yield(p.name, p.id) {
				return
			}
		}
	}), nil
}
