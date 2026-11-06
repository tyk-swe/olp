package projects

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

// CreateProject creates a project managed by its creator, so every project
// starts with the manager it must keep however it is created.
func CreateProject(ctx context.Context, tx pgx.Tx, name, creator string) (id, etag string, err error) {
	id, etag = access.NewID(), access.NewID()
	if _, err = tx.Exec(ctx, "INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1,$2,$3,$4)", id, name, etag, creator); err != nil {
		if duplicateName(err) {
			return "", "", access.Fail(409, "project_name_taken", "A project with this name already exists.")
		}
		return "", "", err
	}
	if _, err = tx.Exec(ctx, "INSERT INTO olp.project_members(project_id,user_id,role,added_by) VALUES($1,$2,'manager',$3)", id, creator, creator); err != nil {
		return "", "", err
	}
	return id, etag, nil
}

func nullableOrganizationPath(r *http.Request) *string {
	if id := r.PathValue("organization_id"); id != "" {
		return &id
	}
	return nil
}

func CreateProjectInOrganization(ctx context.Context, tx pgx.Tx, name, creator string, organization *string) (string, string, error) {
	if organization == nil {
		return CreateProject(ctx, tx, name, creator)
	}
	id, etag := access.NewID(), access.NewID()
	_, err := tx.Exec(ctx, "INSERT INTO olp.projects(id,name,etag,created_by,organization_id) VALUES($1,$2,$3,$4,$5)", id, name, etag, creator, organization)
	if duplicateName(err) {
		err = access.Fail(409, "project_name_taken", "A project with this name already exists.")
	}
	if err == nil {
		_, err = access.AdvanceAuthority(ctx, tx)
	}
	return id, etag, err
}
