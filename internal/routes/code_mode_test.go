package routes_test

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routes"
)

func TestCodeConfigurationRouteRequiresProjectManagement(t *testing.T) {
	const project = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	principal := access.Principal{Projects: map[string]string{project: "developer"}}
	for name, save := range map[string]func() error{
		"draft": func() error {
			_, err := routes.SaveCodeRoute(t.Context(), nil, principal, project, project, routes.CodeRouteInput{ProjectID: project, Slug: "coding", PoolID: project, Models: []string{"native"}, Enabled: true}, false)
			return err
		},
		"publication": func() error {
			_, err := routes.SaveCodePublication(t.Context(), nil, principal, project, codemode.Route{ProjectID: project, Slug: "coding", PoolID: project, Models: []string{"native"}, Enabled: true}, nil, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			var problem *access.Problem
			if err := save(); !errors.As(err, &problem) || problem.Status != 403 {
				t.Fatalf("project member changed coding route policy, got %v", err)
			}
		})
	}
}
