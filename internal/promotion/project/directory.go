package project

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

func ReadDirectory(ctx context.Context, q access.Queryer) (promotion.Directory, error) {
	names, err := projects.ReadProjectNames(ctx, q)
	if err != nil {
		return promotion.Directory{}, err
	}
	return promotion.DirectoryOf(names), nil
}
