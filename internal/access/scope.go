package access

import (
	"context"
	"slices"
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
