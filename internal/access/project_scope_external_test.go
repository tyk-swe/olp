package access_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

func TestProjectAuthorityForInvestigations(t *testing.T) {
	project := "project"
	other := "other"
	viewer := authz.Principal{Projects: map[string]string{project: "viewer"}}
	manager := authz.Principal{Projects: map[string]string{project: "manager"}}
	revoked := authz.Principal{Projects: map[string]string{other: "viewer"}}
	global := authz.Principal{AllProjects: true}

	for _, tc := range []struct {
		name      string
		principal authz.Principal
		project   *string
		need      authz.Need
		status    int
	}{
		{"viewer reads assigned project", viewer, &project, authz.View, 0},
		{"viewer cannot save", viewer, &project, authz.Change, http.StatusForbidden},
		{"manager reads assigned project", manager, &project, authz.View, 0},
		{"manager can save", manager, &project, authz.Change, 0},
		{"foreign project is hidden", manager, &other, authz.View, http.StatusNotFound},
		{"revoked project is hidden", revoked, &project, authz.View, http.StatusNotFound},
		{"unassigned resource is hidden", viewer, nil, authz.View, http.StatusNotFound},
		{"global principal can save", global, &other, authz.Change, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.principal.Project(tc.project, tc.need)
			if tc.status == 0 {
				if err != nil {
					t.Fatalf("project authority: %v", err)
				}
				return
			}
			var problem *httpkit.Problem
			if !errors.As(err, &problem) || problem.Status != tc.status {
				t.Fatalf("project authority returned %v, want status %d", err, tc.status)
			}
		})
	}
}

func TestProjectFilterAdmitsOnlyACanonicalProjectInScope(t *testing.T) {
	project := "123e4567-e89b-12d3-a456-426614174000"
	other := "123e4567-e89b-12d3-a456-426614174001"
	viewer := authz.Principal{Projects: map[string]string{project: "viewer"}}
	global := authz.Principal{AllProjects: true}

	for _, tc := range []struct {
		name      string
		principal authz.Principal
		query     string
		want      string
		status    int
	}{
		{"no filter", viewer, "", "", 0},
		{"assigned project", viewer, "?project_id=" + project, project, 0},
		{"any project for a global principal", global, "?project_id=" + other, other, 0},
		{"foreign project is hidden", viewer, "?project_id=" + other, "", http.StatusNotFound},
		{"upper case", global, "?project_id=123E4567-E89B-12D3-A456-426614174000", "", http.StatusUnprocessableEntity},
		{"nil UUID", global, "?project_id=00000000-0000-0000-0000-000000000000", "", http.StatusUnprocessableEntity},
		{"not a UUID", global, "?project_id=project", "", http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.principal.ProjectFilter(httptest.NewRequest(http.MethodGet, "/"+tc.query, nil))
			if tc.status == 0 {
				if err != nil || got != tc.want {
					t.Fatalf("project filter returned %q, %v, want %q", got, err, tc.want)
				}
				return
			}
			var problem *httpkit.Problem
			if !errors.As(err, &problem) || problem.Status != tc.status {
				t.Fatalf("project filter returned %v, want status %d", err, tc.status)
			}
		})
	}
}
