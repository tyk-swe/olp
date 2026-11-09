package access

import "context"

func ActiveProjectMember(ctx context.Context, q Queryer, projectID, userID string) (bool, error) {
	var member bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.effective_project_members m JOIN olp.users u ON u.id=m.user_id WHERE m.project_id=$1 AND m.user_id=$2 AND u.active)`, projectID, userID).Scan(&member)
	return member, err
}

func ProjectKeyUsable(ctx context.Context, q Queryer, keyID, projectID string, createdBy *string) (bool, error) {
	var usable bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.api_keys WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL AND ($3::uuid IS NULL OR created_by=$3))`, keyID, projectID, createdBy).Scan(&usable)
	return usable, err
}
