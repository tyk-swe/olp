package route

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

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
	groups []group
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
	for _, entry := range document.Routes {
		if policy, ok := promotion.Present(entry.RoutingPolicy); ok {
			if err := validatePolicyMembers(policy); err != nil {
				return Desired{}, err
			}
		}
	}
	declared, err := promotion.Convert[[]Route](document.Routes)
	if err != nil {
		return Desired{}, err
	}
	parsed := promotion.Map(declared, trimmed)
	providers := declaredProviders(document.Providers)
	first := promotion.FirstIndex(parsed, slugOf)
	if err := promotion.EveryIndex(parsed, func(i int, r Route) error { return validate(i, r, first, providers) }); err != nil {
		return Desired{}, err
	}
	templates, err := parseTemplates(document.Templates)
	if err != nil {
		return Desired{}, err
	}
	return Desired{Configuration: Configuration{Routes: parsed, Templates: templates}, groups: groupsOf(document.Projects)}, nil
}

func trimmed(r Route) Route {
	return Route{
		Behavior: r.Behavior, Slug: strings.TrimSpace(r.Slug), Project: promotion.Trimmed(r.Project), Operations: r.Operations,
		OverallTimeoutMS: r.OverallTimeoutMS, MaxAttempts: r.MaxAttempts, RoutingPolicy: r.RoutingPolicy, ContentPolicy: r.ContentPolicy,
		Fidelity: r.Fidelity, ExposeUpstreamModels: r.ExposeUpstreamModels, Retired: r.Retired,
		Targets: promotion.Map(r.Targets, func(t Target) Target {
			return Target{Provider: strings.TrimSpace(t.Provider), ProviderModel: strings.TrimSpace(t.ProviderModel), Priority: t.Priority, Weight: t.Weight, TimeoutMS: t.TimeoutMS, Tags: t.Tags, Shadow: t.Shadow}
		}),
	}
}

func declaredProviders(entries []contract.ConfigurationProviderEntry) map[string]provider {
	return promotion.Index(promotion.Map(entries, func(e contract.ConfigurationProviderEntry) provider {
		return provider{
			name:    strings.ToLower(strings.TrimSpace(e.Name)),
			project: promotion.Trimmed(promotion.Pointer(e.Project)),
			models:  promotion.Map(e.Models, func(m contract.ConfigurationModelEntry) string { return strings.TrimSpace(m.UpstreamModel) }),
		}
	}), func(p provider) string { return p.name })
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
	return promotion.EveryIndex(r.Targets, func(j int, t Target) error { return validateTarget(prefix, j, t, r.Project, providers) })
}

func validatePassthrough(r Route, fidelity routing.RouteFidelity) error {
	policy, err := decodeContentPolicy(r.ContentPolicy)
	if err != nil {
		return err
	}
	route := routing.Route{
		Slug: r.Slug, Operations: r.Operations, OverallTimeout: int64(r.OverallTimeoutMS), MaxAttempts: r.MaxAttempts, Fidelity: fidelity, Behavior: r.Behavior, ContentPolicy: policy,
		Targets: promotion.Map(r.Targets, func(t Target) routing.Target {
			return routing.Target{ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, Timeout: int64(t.TimeoutMS), Tags: t.Tags, Shadow: t.Shadow}
		}),
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
	if !strings.EqualFold(promotion.ValueOf(project), promotion.ValueOf(declared.project)) {
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
	sorted := slices.SortedFunc(slices.Values(c.Routes), func(a, b Route) int { return strings.Compare(a.Slug, b.Slug) })
	converted, err := promotion.Convert[[]contract.ConfigurationRouteEntry](promotion.Map(sorted, withoutContentPolicy))
	if err != nil {
		return nil, nil, err
	}
	rendered := promotion.NonNil(converted)
	if len(c.Templates) == 0 {
		return rendered, nil, nil
	}
	templates, err := promotion.Convert[[]contract.ConfigurationTemplateEntry](slices.SortedFunc(slices.Values(c.Templates), func(a, b Template) int { return strings.Compare(a.Name, b.Name) }))
	if err != nil {
		return nil, nil, err
	}
	return rendered, &templates, nil
}

func withoutContentPolicy(r Route) Route {
	return Route{
		Behavior: r.Behavior, Slug: r.Slug, Project: r.Project, Operations: r.Operations, OverallTimeoutMS: r.OverallTimeoutMS, MaxAttempts: r.MaxAttempts,
		Targets: r.Targets, RoutingPolicy: r.RoutingPolicy, Fidelity: r.Fidelity, ExposeUpstreamModels: r.ExposeUpstreamModels, Retired: r.Retired,
	}
}

func slugOf(r Route) string { return r.Slug }

func validatePolicyMembers(policy contract.RoutingPolicy) error {
	if _, err := promotion.ConvertUnique[*routing.Preferences](policy.Constraints); err != nil {
		return err
	}
	_, err := promotion.ConvertUnique[*routing.Preferences](policy.Defaults)
	return err
}
