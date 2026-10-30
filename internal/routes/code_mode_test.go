package routes_test

import (
	"errors"
	"strings"
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
			_, err := routes.SaveCodeRoute(t.Context(), nil, principal, project, project, routes.CodeRouteInput{ProjectID: strings.ToUpper(project), Slug: "coding", PoolID: project, Models: []string{"native"}, Enabled: true, CodexCLIIdentity: true}, false)
			return err
		},
		"publication": func() error {
			_, err := routes.SaveCodePublication(t.Context(), nil, principal, project, codemode.Route{ProjectID: strings.ToUpper(project), Slug: "coding", PoolID: project, Models: []string{"native"}, Enabled: true, CodexCLIIdentity: true}, nil, nil)
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

func TestCodeConfigurationRouteNormalizesReferences(t *testing.T) {
	const id = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	input, err := (routes.CodeRouteInput{ProjectID: strings.ToUpper(id), PoolID: strings.ToUpper(id), Slug: "coding", Models: []string{"native", "fallback-native"}, CodexCLIIdentity: true, InitialRecoveryAttempts: 1, PaidParticipation: true, PaidFallbackTargets: map[string]codemode.PaidTarget{"fallback-native": {ProviderID: strings.ToUpper(id), SlotID: strings.ToUpper(id)}}, PaidTargets: map[string]codemode.PaidTarget{"native": {ProviderID: strings.ToUpper(id), SlotID: strings.ToUpper(id)}}, Tools: &codemode.ResearchTools{AccountID: strings.ToUpper(id), Services: []string{"search"}}}).Validated()
	if err != nil {
		t.Fatal(err)
	}
	if !input.CodexCLIIdentity || input.ProjectID != id || input.PoolID != id || input.Tools.AccountID != id || input.PaidTargets["native"].ProviderID != id || input.PaidTargets["native"].SlotID != id || input.PaidFallbackTargets["fallback-native"].ProviderID != id || input.PaidFallbackTargets["fallback-native"].SlotID != id {
		t.Fatalf("validated route kept noncanonical references %+v", input)
	}
}
