package access_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

func TestCodeConfigurationWritesRequireProjectManagement(t *testing.T) {
	const project = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	principal := access.Principal{Projects: map[string]string{project: "developer"}}
	for name, save := range map[string]func() error{
		"pool": func() error {
			_, err := access.SaveCodePool(t.Context(), nil, principal, project, project, access.CodePoolInput{ProjectID: strings.ToUpper(project), Name: "personal work", Kind: "shared", AccountIDs: []string{}, APIKeyIDs: []string{}}, false)
			return err
		},
		"budget": func() error {
			_, err := access.SaveCodeBudget(t.Context(), nil, principal, project, project, access.CodeBudgetInput{ProjectID: strings.ToUpper(project), DailyTokens: new(int64(100)), Enabled: true}, false)
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

func TestCodeConfigurationPoolNormalizesAssignments(t *testing.T) {
	const id = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	input, err := (access.CodePoolInput{ProjectID: strings.ToUpper(id), Name: "pool", Kind: "personal", OwnerUserID: new(strings.ToUpper(id)), AccountIDs: []string{strings.ToUpper(id)}, AccountWeights: map[string]int64{id: 3}, APIKeyIDs: []string{strings.ToUpper(id)}}).Validated()
	if err != nil {
		t.Fatal(err)
	}
	if input.ProjectID != id || *input.OwnerUserID != id || input.AccountIDs[0] != id || input.APIKeyIDs[0] != id || input.AccountWeights[id] != 3 {
		t.Fatalf("validated pool changed UUID assignment semantics %+v", input)
	}
}
