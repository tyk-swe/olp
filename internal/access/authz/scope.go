package authz

import (
	"net/http"
	"slices"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
)

// Need is how a principal uses a project-bound resource.
type Need uint8

const (
	// View reads a resource.
	View Need = iota
	// Change writes a resource, which takes a project manager.
	Change
)

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
		return httpkit.NotFound
	}
	role, ok := p.Projects[*projectID]
	if !ok {
		return httpkit.NotFound
	}
	if need == Change && role != "manager" {
		return httpkit.Forbidden()
	}
	return nil
}

func (p Principal) ProjectFilter(r *http.Request) (string, error) {
	project := r.URL.Query().Get("project_id")
	if project == "" {
		return "", nil
	}
	if !ids.Canonical(project) {
		return "", httpkit.Invalid("project_id", "Use a canonical project UUID.")
	}
	if err := p.Project(&project, View); err != nil {
		return "", err
	}
	return project, nil
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

// Organization membership widens project scope, never installation roles or
// token scopes. Explicitly project-scoped tokens cannot administer an organization.
func (p Principal) Organization(id string, need Need) error {
	if p.AllProjects && (need == View || p.Authorize(Access) == nil) {
		return nil
	}
	role, ok := p.Organizations[id]
	if !ok {
		return httpkit.NotFound
	}
	if need == Change && role != "manager" {
		return httpkit.Forbidden()
	}
	return nil
}

func (p Principal) OrganizationIDs() []string {
	ids := make([]string, 0, len(p.Organizations))
	for id := range p.Organizations {
		ids = append(ids, id)
	}
	return ids
}
