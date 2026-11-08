package pricing

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
)

type directory struct {
	ids   map[string]string
	names map[string]string
}

type named struct{ id, name string }

type destination struct {
	providers     directory
	organizations directory
	projects      project.Directory
}

func readDestination(ctx context.Context, q access.Queryer) (destination, error) {
	providers, err := readDirectory(ctx, q, "SELECT id::text,name FROM olp.providers")
	if err != nil {
		return destination{}, err
	}
	organizations, organizationErr := readDirectory(ctx, q, "SELECT id::text,name FROM olp.organizations")
	if organizationErr != nil {
		return destination{}, organizationErr
	}
	projects, projectErr := project.ReadDirectory(ctx, q)
	if projectErr != nil {
		return destination{}, projectErr
	}
	return destination{providers: providers, organizations: organizations, projects: projects}, nil
}

func readDirectory(ctx context.Context, q access.Queryer, query string) (directory, error) {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return directory{}, err
	}
	entries, collectErr := pgx.CollectRows(rows, func(row pgx.CollectableRow) (named, error) {
		var entry named
		err := row.Scan(&entry.id, &entry.name)
		return entry, err
	})
	if collectErr != nil {
		return directory{}, collectErr
	}
	return directory{
		ids: maps.Collect(func(yield func(string, string) bool) {
			for _, entry := range entries {
				if !yield(strings.ToLower(entry.name), entry.id) {
					return
				}
			}
		}),
		names: maps.Collect(func(yield func(string, string) bool) {
			for _, entry := range entries {
				if !yield(entry.id, entry.name) {
					return
				}
			}
		}),
	}, nil
}

func (d directory) id(name string) (string, bool) {
	id, ok := d.ids[strings.ToLower(name)]
	return id, ok
}

func (d directory) resolves(name *string) bool {
	_, ok := d.id(valueOf(name))
	return name == nil || ok
}

func (d directory) idOf(name *string) *string {
	id, ok := d.id(valueOf(name))
	return nonNil(id, name != nil && ok)
}

func (d directory) nameOf(id *string) *string {
	name, ok := d.names[valueOf(id)]
	return nonNil(name, id != nil && ok)
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
	if i := slices.IndexFunc(valueOf(desired.Pricing).Prices, func(price Price) bool { return !providerDeclared(price.Provider) }); i >= 0 {
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
