package project

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/promotion"
)

type Directory struct {
	ids   map[string]string
	names map[string]string
}

type row struct{ id, name string }

func ReadDirectory(ctx context.Context, q access.Queryer) (Directory, error) {
	projects, err := promotion.Rows(ctx, q, func(scanned pgx.CollectableRow) (row, error) {
		var project row
		err := scanned.Scan(&project.id, &project.name)
		return project, err
	}, "SELECT id::text,name FROM olp.projects")
	if err != nil {
		return Directory{}, err
	}
	return Directory{
		ids:   promotion.Associate(projects, func(project row) (string, string) { return promotion.Fold(project.name), project.id }),
		names: promotion.Associate(projects, func(project row) (string, string) { return project.id, project.name }),
	}, nil
}

func (d Directory) ID(name string) (string, bool) {
	id, ok := d.ids[promotion.Fold(name)]
	return id, ok
}

func (d Directory) Name(id string) (string, bool) {
	name, ok := d.names[id]
	return name, ok
}
