package access

import (
	"context"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Need is how a principal uses a project-bound resource.
type Need uint8

const (
	// View reads a resource.
	View Need = iota
	// Change writes a resource, which takes a project manager.
	Change
)

var notFound = Fail(404, "not_found", "The resource was not found.")

// Project decides p's access to a resource in projectID, where nil is the
// unassigned boundary only installation-wide principals reach. A resource
// outside p's scope answers 404 exactly like one that does not exist, so no
// response reveals another project; a visible resource p may not change
// answers 403.
func (p Principal) Project(projectID *string, need Need) error {
	if p.AllProjects {
		return nil
	}
	if projectID == nil {
		return notFound
	}
	role, ok := p.Projects[*projectID]
	if !ok {
		return notFound
	}
	if need == Change && role != "manager" {
		return Forbidden()
	}
	return nil
}

// ProjectIDs lists the projects p reaches when it is not installation-wide,
// for filtering lists alongside AllProjects.
func (p Principal) ProjectIDs() []string {
	ids := make([]string, 0, len(p.Projects))
	for id := range p.Projects {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// RequireProject checks that p may place a new resource in projectID. A
// project that does not exist answers like one outside p's scope, and the
// unassigned boundary takes an installation-wide principal.
func (s *Server) RequireProject(ctx context.Context, q Queryer, p Principal, projectID *string) error {
	if projectID == nil {
		if p.AllProjects {
			return nil
		}
		return Forbidden()
	}
	if err := p.Project(projectID, View); err != nil {
		return err
	}
	var exists bool
	if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.projects WHERE id=$1)", *projectID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return notFound
	}
	return p.Project(projectID, Change)
}

// CreateProject creates a project managed by its creator, so every project
// starts with the manager it must keep however it is created.
func CreateProject(ctx context.Context, tx pgx.Tx, name, creator string) (id, etag string, err error) {
	id, etag = NewID(), NewID()
	if _, err = tx.Exec(ctx, "INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1,$2,$3,$4)", id, name, etag, creator); err != nil {
		if duplicateName(err) {
			return "", "", Fail(409, "project_name_taken", "A project with this name already exists.")
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

// CreateProjectInOrganization creates a project its organization manages, or
// one managed by its creator when it belongs to no organization.
func CreateProjectInOrganization(ctx context.Context, tx pgx.Tx, name, creator string, organization *string) (string, string, error) {
	if organization == nil {
		return CreateProject(ctx, tx, name, creator)
	}
	id, etag := NewID(), NewID()
	_, err := tx.Exec(ctx, "INSERT INTO olp.projects(id,name,etag,created_by,organization_id) VALUES($1,$2,$3,$4,$5)", id, name, etag, creator, organization)
	if duplicateName(err) {
		err = Fail(409, "project_name_taken", "A project with this name already exists.")
	}
	if err == nil {
		_, err = AdvanceAuthority(ctx, tx)
	}
	return id, etag, err
}
