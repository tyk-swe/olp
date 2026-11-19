package project

import (
	"context"

	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

// ReadDirectory is read again by every planner and applier on purpose. One
// apply runs in a single transaction where the project applier creates
// projects that later appliers resolve by name, so a directory loaded before
// the applier loop would not contain them.
func ReadDirectory(ctx context.Context, q database.Queryer) (promotion.Directory, error) {
	names, err := projects.ReadProjectNames(ctx, q)
	if err != nil {
		return promotion.Directory{}, err
	}
	return promotion.DirectoryOf(names), nil
}
