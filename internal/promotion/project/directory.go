package project

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

type Directory struct {
	ids   map[string]string
	names map[string]string
}

func ReadDirectory(ctx context.Context, q access.Queryer) (Directory, error) {
	named, err := projects.ReadProjectNames(ctx, q)
	if err != nil {
		return Directory{}, err
	}
	return Directory{
		ids:   promotion.Associate(named, func(project projects.Named) (string, string) { return promotion.Fold(project.Name), project.ID }),
		names: promotion.Associate(named, func(project projects.Named) (string, string) { return project.ID, project.Name }),
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
