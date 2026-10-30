package providers_test

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/providers"
)

func TestCodeConfigurationAccountRequiresProjectManagement(t *testing.T) {
	const project = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	principal := access.Principal{Projects: map[string]string{project: "developer"}}
	_, err := providers.SaveCodeAccount(t.Context(), nil, principal, project, project, providers.CodeAccountInput{ProjectID: project, ProviderID: project, CredentialID: project, Name: "account", Models: []string{"native"}, Enabled: true}, false)
	var problem *access.Problem
	if !errors.As(err, &problem) || problem.Status != 403 {
		t.Fatalf("project member changed a coding account, got %v", err)
	}
}
