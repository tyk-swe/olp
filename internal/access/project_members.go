package access

import (
	"context"

	"github.com/tyk-swe/olp/internal/database"
)

func ActiveProjectMember(ctx context.Context, q database.Queryer, projectID, userID string) (bool, error) {
	var member bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.effective_project_members m JOIN olp.users u ON u.id=m.user_id WHERE m.project_id=$1 AND m.user_id=$2 AND u.active)`, projectID, userID).Scan(&member)
	return member, err
}
