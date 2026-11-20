package apikeys

import (
	"context"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/database"
)

func checkBudgetGroup(ctx context.Context, q database.Queryer, groupID, projectID *string) error {
	if groupID == nil {
		return nil
	}
	var groupProject *string
	if err := q.QueryRow(ctx, "SELECT project_id::text FROM olp.budget_groups WHERE id=$1 AND retired_at IS NULL", *groupID).Scan(&groupProject); err != nil {
		return err
	}
	if (groupProject == nil) != (projectID == nil) || (groupProject != nil && *groupProject != *projectID) {
		return httpkit.Invalid("budget_group_id", "Budget group must belong to the key's project.")
	}
	return nil
}
