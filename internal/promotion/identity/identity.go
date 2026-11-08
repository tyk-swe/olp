package identity

import (
	"context"
	"net/http"
	"slices"

	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

type Desired struct {
	saml     *access.SAMLDefinition
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
	Identity *http.Client
}

func Parse(document contract.ConfigurationDocument) (Desired, error) {
	groups, err := parseSCIM(document.ScimGroupMappings)
	if err != nil {
		return Desired{}, err
	}
	definition, err := parseSAML(document.Saml)
	if err != nil {
		return Desired{}, err
	}
	issuers, err := parseWorkload(document.WorkloadIssuers)
	if err != nil {
		return Desired{}, err
	}
	return Desired{saml: definition, groups: groups, issuers: issuers, declared: declaredProjects(document.Projects)}, nil
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

func Render(d Desired) (Fragment, error) {
	definition, err := renderSAML(d.saml)
	if err != nil {
		return Fragment{}, err
	}
	return Fragment{
		Saml:              definition,
		ScimGroupMappings: listed(renderSCIM(d.groups)),
		WorkloadIssuers:   listed(renderWorkload(d.issuers)),
	}, nil
}

func Authorize(d Desired, p access.Principal) error {
	if d.saml == nil && len(d.groups) == 0 && len(d.issuers) == 0 {
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
