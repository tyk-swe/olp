package project

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/routes/modelcatalog"
)

func CatalogApplier(desired []Project) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p authz.Principal) error {
		current, err := Read(ctx, tx)
		if err != nil {
			return err
		}
		if err := authorizeCatalog(p, desired, current); err != nil {
			return err
		}
		for _, project := range publishing(desired, current) {
			existing, ok := current.Project(project.Name)
			if !ok {
				return promotion.UnknownProject(project.Name)
			}
			if _, err := modelcatalog.SetProjectPublication(ctx, tx, existing.ID, project.PublicCatalog, project.PublicCatalogPrices); err != nil {
				return err
			}
		}
		return nil
	}
}

func TemplateApplier(desired []Project) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, _ authz.Principal) error {
		current, err := Read(ctx, tx)
		if err != nil {
			return err
		}
		for _, project := range desired {
			existing, ok := current.Project(project.Name)
			if !ok {
				return promotion.UnknownProject(project.Name)
			}
			if err := projects.ValidateTemplateReferences(ctx, tx, existing.ID, existing.Policies.Templates); err != nil {
				return err
			}
		}
		return nil
	}
}
