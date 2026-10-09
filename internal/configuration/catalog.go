package configuration

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type publicationFlags struct{ enabled, prices bool }

func catalogPublications(ctx context.Context, q access.Queryer) (map[string]publicationFlags, error) {
	rows, err := q.Query(ctx, `SELECT lower(p.name),c.enabled,c.prices_public FROM olp.model_catalog_project_settings c JOIN olp.projects p ON p.id=c.project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]publicationFlags{}
	for rows.Next() {
		var name string
		var flags publicationFlags
		if err = rows.Scan(&name, &flags.enabled, &flags.prices); err != nil {
			return nil, err
		}
		result[name] = flags
	}
	return result, rows.Err()
}

func authorizeCatalogPublication(ctx context.Context, q access.Queryer, p access.Principal, doc *Document) error {
	current, err := catalogPublications(ctx, q)
	if err != nil {
		return err
	}
	for _, project := range doc.Projects {
		if current[strings.ToLower(project.Name)] != (publicationFlags{project.PublicCatalog, project.PublicCatalogPrices}) {
			return p.Authorize(access.Access)
		}
	}
	return nil
}

func planCatalogPolicies(ctx context.Context, q access.Queryer, doc *Document, result *planResult) error {
	current, err := catalogPublications(ctx, q)
	if err != nil {
		return err
	}
	for _, project := range doc.Projects {
		if current[strings.ToLower(project.Name)] != (publicationFlags{project.PublicCatalog, project.PublicCatalogPrices}) {
			result.item("catalog_publication", project.Name, "replace", "")
		}
	}
	return nil
}

func applyCatalogPolicies(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document) error {
	if err := authorizeCatalogPublication(ctx, tx, p, doc); err != nil {
		return err
	}
	for _, project := range doc.Projects {
		var id string
		var enabled, prices bool
		if err := tx.QueryRow(ctx, `SELECT p.id::text,COALESCE(c.enabled,false),COALESCE(c.prices_public,false) FROM olp.projects p LEFT JOIN olp.model_catalog_project_settings c ON c.project_id=p.id WHERE lower(p.name)=lower($1)`, project.Name).Scan(&id, &enabled, &prices); err != nil {
			return err
		}
		if enabled == project.PublicCatalog && prices == project.PublicCatalogPrices {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO olp.model_catalog_project_settings(project_id,enabled,prices_public,etag) VALUES($1,$2,$3,$4) ON CONFLICT(project_id) DO UPDATE SET enabled=excluded.enabled,prices_public=excluded.prices_public,etag=excluded.etag`, id, project.PublicCatalog, project.PublicCatalogPrices, access.NewID()); err != nil {
			return err
		}
	}
	return nil
}
