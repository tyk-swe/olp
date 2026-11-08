package project

import (
	"context"
	"maps"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type Directory struct {
	ids   map[string]string
	names map[string]string
}

type row struct{ id, name string }

func ReadDirectory(ctx context.Context, q access.Queryer) (Directory, error) {
	rows, err := q.Query(ctx, "SELECT id::text,name FROM olp.projects")
	if err != nil {
		return Directory{}, err
	}
	projects, err := pgx.CollectRows(rows, func(scanned pgx.CollectableRow) (row, error) {
		var project row
		err := scanned.Scan(&project.id, &project.name)
		return project, err
	})
	if err != nil {
		return Directory{}, err
	}
	return Directory{
		ids:   index(projects, func(project row) (string, string) { return strings.ToLower(project.name), project.id }),
		names: index(projects, func(project row) (string, string) { return project.id, project.name }),
	}, nil
}

func (d Directory) ID(name string) (string, bool) {
	id, ok := d.ids[strings.ToLower(name)]
	return id, ok
}

func (d Directory) Name(id string) (string, bool) {
	name, ok := d.names[id]
	return name, ok
}

func index(projects []row, entry func(row) (string, string)) map[string]string {
	return maps.Collect(func(yield func(string, string) bool) {
		for _, project := range projects {
			if !yield(entry(project)) {
				return
			}
		}
	})
}
