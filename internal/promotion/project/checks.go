package project

import (
	"context"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/modelcatalog"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

func RouteGroupApplier(desired []Project) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, _ access.Principal) error {
		directory, err := ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		for _, project := range desired {
			id, _ := directory.ID(project.Name)
			if err := checkGroupOwner(ctx, tx, id, project); err != nil {
				return err
			}
		}
		return nil
	}
}

func checkGroupOwner(ctx context.Context, tx pgx.Tx, id string, project Project) error {
	for _, group := range slices.Sorted(maps.Keys(project.RouteGroups)) {
		var foreign bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (SELECT slug,project_id FROM olp.routes UNION ALL SELECT slug,project_id FROM olp.code_routes) r WHERE slug=ANY($1::text[]) AND project_id IS DISTINCT FROM $2::uuid)`, project.RouteGroups[group], id).Scan(&foreign); err != nil {
			return err
		}
		if foreign {
			return foreignRoutes()
		}
	}
	return nil
}

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
