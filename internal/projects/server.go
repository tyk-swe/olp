package projects

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

type Server struct {
	access *access.Server
}

func New(auth *access.Server) *Server { return &Server{access: auth} }

func (s *Server) Register(mux *http.ServeMux) {
	for pattern, handler := range map[string]access.Handler{
		"GET /api/v1/projects/{project_id}/limit-templates":                                      s.projectLimitTemplates,
		"PUT /api/v1/projects/{project_id}/limit-templates":                                      s.putProjectLimitTemplates,
		"GET /api/v1/projects/{project_id}/route-groups":                                         s.projectRouteGroups,
		"PUT /api/v1/projects/{project_id}/route-groups":                                         s.putProjectRouteGroups,
		"GET /api/v1/projects/{project_id}/attribution-budgets":                                  s.projectAttributionBudgets,
		"PUT /api/v1/projects/{project_id}/attribution-budgets":                                  s.putProjectAttributionBudgets,
		"GET /api/v1/projects/{project_id}/budget":                                               s.projectBudget,
		"PUT /api/v1/projects/{project_id}/budget":                                               s.putProjectBudget,
		"GET /api/v1/budgets/installation":                                                       s.installationBudget,
		"PUT /api/v1/budgets/installation":                                                       s.putInstallationBudget,
		"GET /api/v1/projects/{project_id}/attribution-policy":                                   s.projectAttributionPolicy,
		"PUT /api/v1/projects/{project_id}/attribution-policy":                                   s.putProjectAttributionPolicy,
		"GET /api/v1/projects/{project_id}/end-user-policy":                                      s.projectEndUserPolicy,
		"PUT /api/v1/projects/{project_id}/end-user-policy":                                      s.putProjectEndUserPolicy,
		"GET /api/v1/organizations":                                                              s.organizations,
		"POST /api/v1/organizations":                                                             s.createOrganization,
		"GET /api/v1/organizations/{organization_id}":                                            s.organization,
		"PATCH /api/v1/organizations/{organization_id}":                                          s.updateOrganization,
		"GET /api/v1/organizations/{organization_id}/members":                                    s.organizationMembers,
		"PUT /api/v1/organizations/{organization_id}/members/{user_id}":                          s.putOrganizationMember,
		"DELETE /api/v1/organizations/{organization_id}/members/{user_id}":                       s.deleteOrganizationMember,
		"GET /api/v1/organizations/{organization_id}/projects":                                   s.projects,
		"POST /api/v1/organizations/{organization_id}/projects":                                  s.createProject,
		"GET /api/v1/organizations/{organization_id}/projects/{project_id}":                      s.project,
		"PATCH /api/v1/organizations/{organization_id}/projects/{project_id}":                    s.updateProject,
		"GET /api/v1/organizations/{organization_id}/projects/{project_id}/members":              s.projectMembers,
		"PUT /api/v1/organizations/{organization_id}/projects/{project_id}/members/{user_id}":    s.putProjectMember,
		"DELETE /api/v1/organizations/{organization_id}/projects/{project_id}/members/{user_id}": s.deleteProjectMember,
		"GET /api/v1/organizations/{organization_id}/budget":                                     s.installationBudget,
		"PUT /api/v1/organizations/{organization_id}/budget":                                     s.putInstallationBudget,
		"GET /api/v1/projects":                                                                   s.projects,
		"POST /api/v1/projects":                                                                  s.createProject,
		"GET /api/v1/project-memberships":                                                        s.projectMemberships,
		"GET /api/v1/projects/{project_id}":                                                      s.project,
		"PATCH /api/v1/projects/{project_id}":                                                    s.updateProject,
		"DELETE /api/v1/projects/{project_id}":                                                   s.deleteProject,
		"GET /api/v1/projects/{project_id}/members":                                              s.projectMembers,
		"PUT /api/v1/projects/{project_id}/members/{user_id}":                                    s.putProjectMember,
		"DELETE /api/v1/projects/{project_id}/members/{user_id}":                                 s.deleteProjectMember,
	} {
		s.access.Route(mux, pattern, handler)
	}
}
