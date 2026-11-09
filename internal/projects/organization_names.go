package projects

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
)

func OrganizationName(ctx context.Context, q access.Queryer, id string) (string, error) {
	var name string
	err := q.QueryRow(ctx, "SELECT name FROM olp.organizations WHERE id=$1", id).Scan(&name)
	return name, err
}
