package project

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/modelcatalog"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

func CatalogApplier(desired []Project) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		current, err := Read(ctx, tx)
		if err != nil {
			return err
		}
		if err := authorizeCatalog(p, desired, current); err != nil {
			return err
		}
		for _, project := range publishing(desired, current) {
			existing, _ := current.Project(project.Name)
			if _, err := modelcatalog.SetProjectPublication(ctx, tx, existing.ID, project.PublicCatalog, project.PublicCatalogPrices); err != nil {
				return err
			}
		}
		return nil
	}
}

func TemplateApplier(desired []Project) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, _ access.Principal) error {
		current, err := Read(ctx, tx)
		if err != nil {
			return err
		}
		for _, project := range desired {
			existing, _ := current.Project(project.Name)
			if err := projects.ValidateTemplateReferences(ctx, tx, existing.ID, existing.Policies.Templates); err != nil {
				return err
			}
		}
		return nil
	}
}
