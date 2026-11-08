package identity

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

type Desired struct {
	saml     nullable.Nullable[contract.ConfigurationSAML]
	groups   []contract.ConfigurationSCIMGroupMapping
	issuers  []contract.ConfigurationWorkloadIssuer
	declared map[string]contract.ConfigurationProjectEntry
}

type Fragment struct {
	Saml              nullable.Nullable[contract.ConfigurationSAML]
	ScimGroupMappings nullable.Nullable[[]contract.ConfigurationSCIMGroupMapping]
	WorkloadIssuers   nullable.Nullable[[]contract.ConfigurationWorkloadIssuer]
}

type Deps struct {
	Access   *access.Server
	Pool     access.Queryer
	Identity *http.Client
}

func Parse(document contract.ConfigurationDocument) (Desired, error) {
	groups, err := parseSCIM(document.ScimGroupMappings)
	if err != nil {
		return Desired{}, err
	}
	if err := parseSAML(document.Saml); err != nil {
		return Desired{}, err
	}
	issuers, err := parseWorkload(document.WorkloadIssuers)
	if err != nil {
		return Desired{}, err
	}
	return Desired{saml: document.Saml, groups: groups, issuers: issuers, declared: declaredProjects(document.Projects)}, nil
}

func Read(ctx context.Context, q access.Queryer) (Desired, error) {
	definition, err := readSAML(ctx, q)
	if err != nil {
		return Desired{}, err
	}
	groups, err := readSCIM(ctx, q)
	if err != nil {
		return Desired{}, err
	}
	directory, err := project.ReadDirectory(ctx, q)
	if err != nil {
		return Desired{}, err
	}
	issuers, err := readWorkload(ctx, q, directory)
	if err != nil {
		return Desired{}, err
	}
	return Desired{saml: definition, groups: groups, issuers: issuers}, nil
}

func Render(d Desired) Fragment {
	return Fragment{
		Saml:              d.saml,
		ScimGroupMappings: listed(renderSCIM(d.groups)),
		WorkloadIssuers:   listed(renderWorkload(d.issuers)),
	}
}

func Authorize(d Desired, p access.Principal) error {
	if _, ok := promotion.Present(d.saml); !ok && len(d.groups) == 0 && len(d.issuers) == 0 {
		return nil
	}
	return p.Authorize(access.Access)
}

func Planner(d Desired) promotion.Planner {
	return promotion.Planner{
		Authorize: func(_ context.Context, _ access.Queryer, p access.Principal) error { return Authorize(d, p) },
		Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
			definition, err := planSAML(ctx, q, d.saml)
			if err != nil {
				return nil, err
			}
			groups, err := planSCIM(ctx, q, d)
			if err != nil {
				return nil, err
			}
			issuers, err := planWorkload(ctx, q, d)
			if err != nil {
				return nil, err
			}
			return slices.Concat(definition, groups, issuers), nil
		},
	}
}

func action(exists, same bool) contract.ConfigurationPlanItemAction {
	switch {
	case !exists:
		return contract.ConfigurationPlanItemActionCreate
	case same:
		return contract.ConfigurationPlanItemActionReuse
	default:
		return contract.ConfigurationPlanItemActionReplace
	}
}

func listed[T any](items []T) nullable.Nullable[[]T] {
	if len(items) == 0 {
		return nullable.Nullable[[]T]{}
	}
	return nullable.NewNullableWithValue(items)
}

func mapped[T, U any](items []T, convert func(T) U) []U {
	return append([]U{}, slices.Collect(func(yield func(U) bool) {
		for _, item := range items {
			if !yield(convert(item)) {
				return
			}
		}
	})...)
}

type result[T any] struct {
	value T
	err   error
}

func collected[T, U any](items []T, convert func(T) (U, error)) ([]U, error) {
	results := slices.Collect(func(yield func(result[U]) bool) {
		for _, item := range items {
			value, err := convert(item)
			if !yield(result[U]{value: value, err: err}) || err != nil {
				return
			}
		}
	})
	if failed := slices.IndexFunc(results, func(r result[U]) bool { return r.err != nil }); failed >= 0 {
		return nil, results[failed].err
	}
	return mapped(results, func(r result[U]) U { return r.value }), nil
}

func firstError[T any](items []T, check func(T) error) error {
	for _, item := range items {
		if err := check(item); err != nil {
			return err
		}
	}
	return nil
}

func unique[T any](items []T, key func(T) string) bool {
	return len(indexed(items, func(item T) (string, bool) { return key(item), true })) == len(items)
}

func indexed[T any, V any](items []T, entry func(T) (string, V)) map[string]V {
	return maps.Collect(func(yield func(string, V) bool) {
		for _, item := range items {
			if !yield(entry(item)) {
				return
			}
		}
	})
}

func keys[V any](values map[string]V) map[string]bool {
	return maps.Collect(func(yield func(string, bool) bool) {
		for key := range values {
			if !yield(key, true) {
				return
			}
		}
	})
}

func folded(name string) string { return strings.ToLower(name) }
