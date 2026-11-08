package route

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routing"
)

type State struct {
	Configuration
	published map[string]Route
	drafts    map[string]draft
	directory project.Directory
}

type draft struct {
	id    string
	route Route
}

func Read(ctx context.Context, q access.Queryer) (State, error) {
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return State{}, err
	}
	published, err := readPublished(ctx, q)
	if err != nil {
		return State{}, err
	}
	templates, err := readTemplates(ctx, q)
	if err != nil {
		return State{}, err
	}
	drafts, err := routes.ReadOpenDrafts(ctx, q)
	if err != nil {
		return State{}, err
	}
	return State{
		Configuration: Configuration{Routes: published, Templates: templates},
		published:     byKey(published, slugOf),
		drafts:        indexDrafts(drafts, directory),
		directory:     directory,
	}, nil
}

func readPublished(ctx context.Context, q access.Queryer) ([]Route, error) {
	rows, err := q.Query(ctx, `SELECT r.slug,pr.name,r.state='retired',v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.routing_policy,v.content_policy,v.fidelity,v.behavior,
		COALESCE((SELECT expose_upstream_models FROM olp.model_catalog_route_settings WHERE route_id=r.id),false)
		FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id LEFT JOIN olp.projects pr ON pr.id=r.project_id ORDER BY r.slug`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Route, error) {
		var r routes.OpenDraft
		var project *string
		var retired bool
		var operations, targets, policy, behavior []byte
		if err := row.Scan(&r.Slug, &project, &retired, &operations, &r.OverallTimeoutMS, &r.MaxAttempts, &targets, &policy, &r.ContentPolicy, &r.Fidelity, &behavior, &r.ExposeUpstreamModels); err != nil {
			return Route{}, err
		}
		decodedOperations, err := decode[[]string](operations)
		if err != nil {
			return Route{}, err
		}
		decodedTargets, err := decode[[]routing.PublishedTarget](targets)
		if err != nil {
			return Route{}, err
		}
		decodedPolicy, err := routes.DecodePolicy(policy)
		if err != nil {
			return Route{}, err
		}
		decodedBehavior, err := routing.DecodeBehavior(behavior)
		if err != nil {
			return Route{}, err
		}
		return routeOf(routes.OpenDraft{
			Slug: r.Slug, Operations: decodedOperations, OverallTimeoutMS: r.OverallTimeoutMS, MaxAttempts: r.MaxAttempts, Targets: decodedTargets,
			ContentPolicy: r.ContentPolicy, Fidelity: r.Fidelity, Behavior: decodedBehavior, ExposeUpstreamModels: r.ExposeUpstreamModels, RoutingPolicy: decodedPolicy,
		}, project, retired), nil
	})
}

func decode[T any](raw []byte) (T, error) {
	var value T
	err := json.Unmarshal(raw, &value)
	return value, err
}

func routeOf(d routes.OpenDraft, project *string, retired bool) Route {
	return Route{
		Behavior: d.Behavior, Slug: d.Slug, Project: project, Operations: d.Operations, OverallTimeoutMS: d.OverallTimeoutMS, MaxAttempts: d.MaxAttempts,
		RoutingPolicy: d.RoutingPolicy, ContentPolicy: d.ContentPolicy, Fidelity: d.Fidelity, ExposeUpstreamModels: d.ExposeUpstreamModels, Retired: retired,
		Targets: slices.Collect(mapped(d.Targets, func(t routing.PublishedTarget) Target {
			return Target{Provider: t.ProviderName, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: int(t.TimeoutMS), Tags: t.Tags, Shadow: t.Shadow}
		})),
	}
}

func indexDrafts(drafts []routes.OpenDraft, directory project.Directory) map[string]draft {
	return maps.Collect(func(yield func(string, draft) bool) {
		for _, d := range drafts {
			name := projectName(directory, d.ProjectID)
			if !yield(draftKey(d.Slug, name), draft{id: d.ID, route: routeOf(d, name, false)}) {
				return
			}
		}
	})
}

func projectName(directory project.Directory, id *string) *string {
	if id == nil {
		return nil
	}
	name, ok := directory.Name(*id)
	if !ok {
		return nil
	}
	return &name
}

func draftKey(slug string, project *string) string {
	return slug + "\x00" + strings.ToLower(deref(project))
}

func projectIDOf(directory project.Directory, name *string) (*string, error) {
	if name == nil {
		return nil, nil
	}
	id, ok := directory.ID(*name)
	if !ok {
		return nil, fmt.Errorf("project %q is not installed", *name)
	}
	return &id, nil
}

func (s State) current(r Route) (Route, bool) {
	if staged, ok := s.drafts[draftKey(r.Slug, r.Project)]; ok {
		return staged.route, true
	}
	published, ok := s.published[r.Slug]
	return published, ok
}

func (s State) unchanged(r Route) (bool, error) {
	current, ok := s.current(r)
	if !ok {
		return false, nil
	}
	left, err := canonical(r)
	if err != nil {
		return false, err
	}
	right, err := canonical(current)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func canonical(r Route) ([]byte, error) {
	fidelity, err := routing.DecodeFidelity(r.Fidelity)
	if err != nil {
		return nil, err
	}
	encodedFidelity, err := json.Marshal(fidelity)
	if err != nil {
		return nil, err
	}
	policy, err := decodeContentPolicy(r.ContentPolicy)
	if err != nil {
		return nil, err
	}
	encodedPolicy, err := encodeContentPolicy(policy)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Route{
		Behavior: r.Behavior, Slug: r.Slug, Operations: slices.Concat([]string{}, slices.Sorted(slices.Values(r.Operations))),
		OverallTimeoutMS: r.OverallTimeoutMS, MaxAttempts: r.MaxAttempts, RoutingPolicy: cmp.Or(r.RoutingPolicy, &routing.Policy{}),
		ContentPolicy: encodedPolicy, Fidelity: encodedFidelity, ExposeUpstreamModels: r.ExposeUpstreamModels,
		Targets: slices.Concat([]Target{}, slices.SortedFunc(mapped(r.Targets, canonicalTarget), func(a, b Target) int {
			return cmp.Or(strings.Compare(strings.ToLower(a.Provider), strings.ToLower(b.Provider)), strings.Compare(a.ProviderModel, b.ProviderModel), cmp.Compare(a.Priority, b.Priority))
		})),
	})
}

func canonicalTarget(t Target) Target {
	return Target{Provider: t.Provider, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: t.TimeoutMS, Tags: slices.Sorted(slices.Values(t.Tags)), Shadow: t.Shadow}
}

func encodeContentPolicy(policy *contentpolicy.Policy) (json.RawMessage, error) {
	if policy == nil {
		return nil, nil
	}
	return json.Marshal(policy)
}
