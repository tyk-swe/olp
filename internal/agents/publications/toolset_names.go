package publications

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
)

func ToolsetName(ctx context.Context, q access.Queryer, id string) (string, error) {
	var name string
	err := q.QueryRow(ctx, "SELECT name FROM olp.agent_publications WHERE id=$1 AND kind='toolset'", id).Scan(&name)
	return name, err
}
