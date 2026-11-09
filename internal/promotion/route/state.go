package route

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/promotion"
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
	stored, err := routes.ReadPublishedRoutes(ctx, q)
	if err != nil {
		return State{}, err
	}
	published := promotion.Map(stored, func(r routes.PublishedRoute) Route {
		return routeOf(r.OpenDraft, projectName(directory, r.ProjectID), r.Retired)
	})
	templates, err := readTemplates(ctx, q, directory)
	if err != nil {
		return State{}, err
	}
	drafts, err := routes.ReadOpenDrafts(ctx, q)
	if err != nil {
		return State{}, err
	}
	return State{
		Configuration: Configuration{Routes: published, Templates: templates},
		published:     promotion.Index(published, slugOf),
		drafts:        indexDrafts(drafts, directory),
		directory:     directory,
	}, nil
}

func routeOf(d routes.OpenDraft, project *string, retired bool) Route {
	return Route{
		Behavior: d.Behavior, Slug: d.Slug, Project: project, Operations: d.Operations, OverallTimeoutMS: d.OverallTimeoutMS, MaxAttempts: d.MaxAttempts,
		RoutingPolicy: d.RoutingPolicy, ContentPolicy: d.ContentPolicy, Fidelity: d.Fidelity, ExposeUpstreamModels: d.ExposeUpstreamModels, Retired: retired,
		Targets: promotion.Map(d.Targets, func(t routing.PublishedTarget) Target {
			return Target{Provider: t.ProviderName, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: int(t.TimeoutMS), Tags: t.Tags, Shadow: t.Shadow}
		}),
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
	return slug + "\x00" + strings.ToLower(promotion.ValueOf(project))
}

func projectIDOf(directory project.Directory, name *string) (*string, error) {
	if name == nil {
		return nil, nil
	}
	id, ok := directory.ID(*name)
	if !ok {
		return nil, promotion.UnknownProject(*name)
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
	return bytes.Equal(left, right) && routes.SamePolicy(r.RoutingPolicy, current.RoutingPolicy), nil
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
		Behavior: r.Behavior, Slug: r.Slug, Operations: slices.Sorted(slices.Values(r.Operations)),
		OverallTimeoutMS: r.OverallTimeoutMS, MaxAttempts: r.MaxAttempts,
		ContentPolicy: encodedPolicy, Fidelity: encodedFidelity, ExposeUpstreamModels: r.ExposeUpstreamModels,
		Targets: slices.SortedFunc(slices.Values(promotion.Map(r.Targets, canonicalTarget)), func(a, b Target) int {
			return cmp.Or(strings.Compare(strings.ToLower(a.Provider), strings.ToLower(b.Provider)), strings.Compare(a.ProviderModel, b.ProviderModel), cmp.Compare(a.Priority, b.Priority))
		}),
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
