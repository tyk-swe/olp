package access_test

import (
	"errors"
	"net/http"
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
