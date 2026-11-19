package access

import (
	"context"

	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/database"
)

func loadOrganizations(ctx context.Context, q database.Queryer, p *authz.Principal, user string) error {
	rows, err := q.Query(ctx, "SELECT organization_id::text,role FROM olp.organization_members WHERE user_id=$1", user)
	if err != nil {
		return err
	}
	defer rows.Close()
	p.Organizations = map[string]string{}
	for rows.Next() {
		var id, role string
		if err = rows.Scan(&id, &role); err != nil {
			return err
		}
		p.Organizations[id] = role
	}
	return rows.Err()
}
