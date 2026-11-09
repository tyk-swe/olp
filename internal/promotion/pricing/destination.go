package pricing

import (
	"context"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/providers"
)

type directory struct {
	ids   map[string]string
	names map[string]string
}

type destination struct {
	providers     directory
	organizations directory
	projects      project.Directory
}

func readDestination(ctx context.Context, q access.Queryer) (destination, error) {
	providerDirectory, err := readProviders(ctx, q)
	if err != nil {
		return destination{}, err
	}
	organizations, organizationErr := projects.ReadOrganizationNames(ctx, q)
	if organizationErr != nil {
		return destination{}, organizationErr
	}
	projectDirectory, projectErr := project.ReadDirectory(ctx, q)
	if projectErr != nil {
		return destination{}, projectErr
	}
	return destination{
		providers:     providerDirectory,
		organizations: directoryOf(organizations, func(o projects.Named) (string, string) { return o.ID, o.Name }),
		projects:      projectDirectory,
	}, nil
}

func readProviders(ctx context.Context, q access.Queryer) (directory, error) {
	names, err := providers.ReadProviderNames(ctx, q)
	if err != nil {
		return directory{}, err
	}
	return directoryOf(names, func(p providers.ProviderName) (string, string) { return p.ID, p.Name }), nil
}

func directoryOf[T any](entries []T, entry func(T) (id, name string)) directory {
	return directory{
		ids: promotion.Associate(entries, func(e T) (string, string) {
			id, name := entry(e)
			return strings.ToLower(name), id
		}),
		names: promotion.Associate(entries, entry),
	}
}

func (d directory) id(name string) (string, bool) {
	id, ok := d.ids[strings.ToLower(name)]
	return id, ok
}

func (d directory) resolves(name *string) bool {
	_, ok := d.id(promotion.ValueOf(name))
	return name == nil || ok
}

func (d directory) idOf(name *string) *string {
	id, ok := d.id(promotion.ValueOf(name))
	return promotion.PointerIf(id, name != nil && ok)
}

func (d directory) nameOf(id *string) *string {
	name, ok := d.names[promotion.ValueOf(id)]
	return promotion.PointerIf(name, id != nil && ok)
}

func declared(document func(string) bool, destination func(string) (string, bool), name *string) bool {
	if name == nil {
		return true
	}
	_, present := destination(*name)
	return present || document(*name)
}

func checkNames(desired Desired, names promotion.Names, at destination) error {
	providerDeclared := func(name *string) bool { return declared(names.Provider, at.providers.id, name) }
	if i := slices.IndexFunc(promotion.ValueOf(desired.Pricing).Prices, func(price Price) bool { return !providerDeclared(price.Provider) }); i >= 0 {
		return undeclaredPrice(i)
	}
	if i := slices.IndexFunc(desired.Discounts, func(discount Discount) bool { return !providerDeclared(discount.Provider) }); i >= 0 {
		return undeclaredDiscount(i)
	}
	for _, card := range desired.RateCards {
		if err := ownerDeclared(card.Owner, names, at); err != nil {
			return err
		}
	}
	return nil
}

func ownerDeclared(owner Owner, names promotion.Names, at destination) error {
	switch owner := owner.(type) {
	case Organization:
		if !declared(names.Organization, at.organizations.id, &owner.Name) {
			return undeclaredOrganization()
		}
	case Project:
		if !declared(names.Project, at.projects.ID, &owner.Name) {
			return undeclaredProject()
		}
	}
	return nil
}

func undeclaredOrganization() error {
	return access.Invalid("rate_cards.owner.organization", "The card names an organization neither the document nor this installation declares.")
}

func undeclaredProject() error {
	return access.Invalid("rate_cards.owner.project", "The card names a project neither the document nor this installation declares.")
}
