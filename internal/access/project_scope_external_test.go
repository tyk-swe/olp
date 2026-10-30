package access_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

func TestProjectAuthorityForInvestigations(t *testing.T) {
	project := "project"
	other := "other"
	viewer := access.Principal{Projects: map[string]string{project: "viewer"}}
	manager := access.Principal{Projects: map[string]string{project: "manager"}}
	revoked := access.Principal{Projects: map[string]string{other: "viewer"}}
	global := access.Principal{AllProjects: true}

	for _, tc := range []struct {
		name      string
		principal access.Principal
		project   *string
		need      access.Need
		status    int
	}{
		{"viewer reads assigned project", viewer, &project, access.View, 0},
		{"viewer cannot save", viewer, &project, access.Change, http.StatusForbidden},
		{"manager reads assigned project", manager, &project, access.View, 0},
		{"manager can save", manager, &project, access.Change, 0},
		{"foreign project is hidden", manager, &other, access.View, http.StatusNotFound},
		{"revoked project is hidden", revoked, &project, access.View, http.StatusNotFound},
		{"unassigned resource is hidden", viewer, nil, access.View, http.StatusNotFound},
		{"global principal can save", global, &other, access.Change, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.principal.Project(tc.project, tc.need)
			if tc.status == 0 {
				if err != nil {
					t.Fatalf("project authority: %v", err)
				}
				return
			}
			var problem *access.Problem
			if !errors.As(err, &problem) || problem.Status != tc.status {
				t.Fatalf("project authority returned %v, want status %d", err, tc.status)
			}
		})
	}
}
