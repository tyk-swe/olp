package access_test

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

func TestCodeConfigurationWritesRequireProjectManagement(t *testing.T) {
	const project = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	principal := access.Principal{Projects: map[string]string{project: "developer"}}
	for name, save := range map[string]func() error{
		"pool": func() error {
			_, err := access.SaveCodePool(t.Context(), nil, principal, project, project, access.CodePoolInput{ProjectID: project, Name: "personal work", Kind: "shared", AccountIDs: []string{}, APIKeyIDs: []string{}}, false)
			return err
		},
		"budget": func() error {
			_, err := access.SaveCodeBudget(t.Context(), nil, principal, project, project, access.CodeBudgetInput{ProjectID: project, DailyTokens: new(int64(100)), Enabled: true}, false)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			var problem *access.Problem
			if err := save(); !errors.As(err, &problem) || problem.Status != 403 {
				t.Fatalf("project member changed coding policy, got %v", err)
			}
		})
	}
}
