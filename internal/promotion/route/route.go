package route

import (
	"bytes"
	"encoding/json"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/oapi-codegen/nullable"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/routeidentity"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	maxRoutes  = 10000
	maxTargets = 64
)

type Target struct {
	Provider      string          `json:"provider"`
	ProviderModel string          `json:"provider_model"`
	Priority      int             `json:"priority"`
	Weight        int64           `json:"weight"`
	TimeoutMS     int             `json:"timeout_ms"`
	Tags          []string        `json:"tags,omitempty"`
	Shadow        *routing.Shadow `json:"shadow,omitempty"`
}

type Route struct {
	routing.Behavior
	Slug                 string          `json:"slug"`
	Project              *string         `json:"project"`
	Operations           []string        `json:"operations"`
	OverallTimeoutMS     int             `json:"overall_timeout_ms"`
	MaxAttempts          int             `json:"max_attempts"`
	Targets              []Target        `json:"targets"`
	RoutingPolicy        *routing.Policy `json:"routing_policy"`
	ContentPolicy        json.RawMessage `json:"content_policy,omitempty"`
	Fidelity             json.RawMessage `json:"fidelity,omitempty"`
	ExposeUpstreamModels bool            `json:"expose_upstream_models,omitempty"`
	Retired              bool            `json:"retired,omitempty"`
}

type Configuration struct {
	Routes    []Route
	Templates []Template
}

type Desired struct {
	Configuration
	groups   []group
	projects []string
}

type provider struct {
	name    string
	project *string
	models  []string
}

func Parse(document contract.ConfigurationDocument) (Desired, error) {
	if len(document.Routes) > maxRoutes {
		return Desired{}, access.Invalid("routes", "Declare at most "+strconv.Itoa(maxRoutes)+" routes.")
	}
	declared, err := promotion.Convert[[]Route](document.Routes)
	if err != nil {
		return Desired{}, err
	}
	parsed := slices.Collect(mapped(declared, trimmed))
	groups, err := parseGroups(document.Projects, parsed)
	if err != nil {
		return Desired{}, err
	}
	providers := declaredProviders(document.Providers)
	first := firstIndex(parsed, slugOf)
	if err := every(parsed, func(i int, r Route) error { return validate(i, r, first, providers) }); err != nil {
		return Desired{}, err
	}
	templates, err := parseTemplates(document.Templates)
	if err != nil {
		return Desired{}, err
	}
	names := slices.Collect(mapped(document.Projects, func(p contract.ConfigurationProjectEntry) string { return strings.TrimSpace(p.Name) }))
	return Desired{Configuration: Configuration{Routes: parsed, Templates: templates}, groups: groups, projects: names}, nil
}

func trimmed(r Route) Route {
	return Route{
		Behavior: r.Behavior, Slug: strings.TrimSpace(r.Slug), Project: trimmedName(r.Project), Operations: r.Operations,
		OverallTimeoutMS: r.OverallTimeoutMS, MaxAttempts: r.MaxAttempts, RoutingPolicy: r.RoutingPolicy, ContentPolicy: r.ContentPolicy,
		Fidelity: r.Fidelity, ExposeUpstreamModels: r.ExposeUpstreamModels, Retired: r.Retired,
		Targets: slices.Collect(mapped(r.Targets, func(t Target) Target {
			return Target{Provider: strings.TrimSpace(t.Provider), ProviderModel: strings.TrimSpace(t.ProviderModel), Priority: t.Priority, Weight: t.Weight, TimeoutMS: t.TimeoutMS, Tags: t.Tags, Shadow: t.Shadow}
		})),
	}
}

func declaredProviders(entries []contract.ConfigurationProviderEntry) map[string]provider {
	return byKey(slices.Collect(mapped(entries, func(e contract.ConfigurationProviderEntry) provider {
		return provider{
			name:    strings.ToLower(strings.TrimSpace(e.Name)),
			project: presentName(e.Project),
			models:  slices.Collect(mapped(e.Models, func(m contract.ConfigurationModelEntry) string { return strings.TrimSpace(m.UpstreamModel) })),
		}
	})), func(p provider) string { return p.name })
}

func validate(i int, r Route, first map[string]int, providers map[string]provider) error {
	prefix := "routes." + strconv.Itoa(i)
	fidelity, err := routing.DecodeFidelity(r.Fidelity)
	if err != nil {
		return access.Invalid(prefix+".fidelity", err.Error())
	}
	if err := (routing.Route{Fidelity: fidelity, Behavior: r.Behavior}).ValidateCaching(); err != nil {
		return access.Invalid(prefix+".cache", err.Error())
	}
	if err := routes.ValidateFidelityPolicy(r.Fidelity, r.ContentPolicy); err != nil {
		return err
	}
	if first[r.Slug] != i {
		return access.Invalid(prefix+".slug", "Route slugs must be unique.")
	}
	if !routeidentity.ValidSlug(r.Slug) {
		return access.Invalid(prefix+".slug", "Use 1-100 lowercase letters, digits, dots, underscores, or hyphens, starting with a letter or digit.")
	}
	if len(r.Targets) > maxTargets {
		return access.Invalid(prefix+".targets", "Declare at most "+strconv.Itoa(maxTargets)+" targets per route.")
	}
	if r.Project != nil {
		if err := access.ValidText(prefix+".project", *r.Project, 100); err != nil {
			return err
		}
	}
	if r.RoutingPolicy != nil {
		if err := r.RoutingPolicy.Validate(); err != nil {
			return err
		}
	}
	tagged := func(tag string) bool {
		return slices.ContainsFunc(r.Targets, func(t Target) bool { return t.Shadow == nil && slices.Contains(t.Tags, tag) })
	}
	if err := r.Behavior.Validate(r.Slug, tagged); err != nil {
		return err
	}
	if err := validatePassthrough(r, fidelity); err != nil {
		return err
	}
	return every(r.Targets, func(j int, t Target) error { return validateTarget(prefix, j, t, r.Project, providers) })
}

func validatePassthrough(r Route, fidelity routing.RouteFidelity) error {
	policy, err := decodeContentPolicy(r.ContentPolicy)
	if err != nil {
		return err
	}
	route := routing.Route{
		Slug: r.Slug, Operations: r.Operations, OverallTimeout: int64(r.OverallTimeoutMS), MaxAttempts: r.MaxAttempts, Fidelity: fidelity, Behavior: r.Behavior, ContentPolicy: policy,
		Targets: slices.Collect(mapped(r.Targets, func(t Target) routing.Target {
			return routing.Target{ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, Timeout: int64(t.TimeoutMS), Tags: t.Tags, Shadow: t.Shadow}
		})),
	}
	if err := route.ValidatePassthrough(); err != nil {
		return routes.PassthroughAccessError(err)
	}
	return nil
}

func validateTarget(prefix string, j int, t Target, project *string, providers map[string]provider) error {
	field, index := prefix+".targets."+strconv.Itoa(j), strconv.Itoa(j)
	declared, ok := providers[strings.ToLower(t.Provider)]
	if !ok {
		return access.Invalid(field+".provider", "Target "+index+" names a provider the document does not declare.")
	}
	if !slices.Contains(declared.models, t.ProviderModel) {
		return access.Invalid(field+".provider_model", "Target "+index+" names a model the provider does not declare.")
	}
	if !strings.EqualFold(deref(project), deref(declared.project)) {
		return access.Fail(422, "target_project_mismatch", "Target "+index+" belongs to a different project than the route.")
	}
	return nil
}

func decodeContentPolicy(raw json.RawMessage) (*contentpolicy.Policy, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	return contentpolicy.Decode(raw)
}

func Render(c Configuration) ([]contract.ConfigurationRouteEntry, *[]contract.ConfigurationTemplateEntry, error) {
	rendered, err := promotion.Convert[[]contract.ConfigurationRouteEntry](slices.SortedFunc(slices.Values(c.Routes), func(a, b Route) int { return strings.Compare(a.Slug, b.Slug) }))
	if err != nil {
		return nil, nil, err
	}
	if len(c.Templates) == 0 {
		return slices.Concat([]contract.ConfigurationRouteEntry{}, rendered), nil, nil
	}
	templates, err := promotion.Convert[[]contract.ConfigurationTemplateEntry](slices.SortedFunc(slices.Values(c.Templates), func(a, b Template) int { return strings.Compare(a.Name, b.Name) }))
	if err != nil {
		return nil, nil, err
	}
	return slices.Concat([]contract.ConfigurationRouteEntry{}, rendered), &templates, nil
}

func mapped[T, U any](items []T, convert func(T) U) iter.Seq[U] {
	return func(yield func(U) bool) {
		for _, item := range items {
			if !yield(convert(item)) {
				return
			}
		}
	}
}

type outcome[U any] struct {
	value U
	err   error
}

func each[T, U any](items []T, parse func(int, T) (U, error)) ([]U, error) {
	outcomes := slices.Collect(func(yield func(outcome[U]) bool) {
		for i, item := range items {
			value, err := parse(i, item)
			if !yield(outcome[U]{value: value, err: err}) || err != nil {
				return
			}
		}
	})
	if len(outcomes) > 0 && outcomes[len(outcomes)-1].err != nil {
		return nil, outcomes[len(outcomes)-1].err
	}
	return slices.Collect(mapped(outcomes, func(o outcome[U]) U { return o.value })), nil
}

func every[T any](items []T, check func(int, T) error) error {
	_, err := each(items, func(i int, item T) (struct{}, error) { return struct{}{}, check(i, item) })
	return err
}

func byKey[T any](items []T, key func(T) string) map[string]T {
	return maps.Collect(func(yield func(string, T) bool) {
		for _, item := range items {
			if !yield(key(item), item) {
				return
			}
		}
	})
}

func firstIndex[T any](items []T, key func(T) string) map[string]int {
	return maps.Collect(func(yield func(string, int) bool) {
		for i := len(items) - 1; i >= 0; i-- {
			if !yield(key(items[i]), i) {
				return
			}
		}
	})
}

func slugOf(r Route) string { return r.Slug }

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func trimmedName(value *string) *string {
	if value == nil {
		return nil
	}
	name := strings.TrimSpace(*value)
	return &name
}

func presentName(value nullable.Nullable[string]) *string {
	name, ok := promotion.Present(value)
	if !ok {
		return nil
	}
	return trimmedName(&name)
}
