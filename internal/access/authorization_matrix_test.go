package access

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/access/authz"
)

func TestManagementAuthorizationGoldenMatrix(t *testing.T) {
	raw, err := os.ReadFile("testdata/authorization.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Archetypes []struct {
			Name        string   `json:"name"`
			Kind        string   `json:"kind"`
			Role        string   `json:"role"`
			CreatorRole string   `json:"creator_role"`
			Global      bool     `json:"global"`
			Scopes      []string `json:"scopes"`
		} `json:"archetypes"`
		Routes map[string]string `json:"routes"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	requirements, err := ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	for pattern, req := range requirements {
		if !slices.ContainsFunc(req.Alternatives, func(a Alternative) bool { return a.Kind == "machine" }) {
			continue
		}
		expected, ok := golden.Routes[pattern]
		if !ok {
			t.Errorf("%s has no authorization golden", pattern)
			continue
		}
		if len(expected) != len(golden.Archetypes) {
			t.Errorf("%s has %d entries for %d archetypes", pattern, len(expected), len(golden.Archetypes))
			continue
		}
		for i, archetype := range golden.Archetypes {
			if expected[i] != 'Y' && expected[i] != '-' {
				t.Errorf("%s: invalid admission marker %q", pattern, expected[i])
				continue
			}
			p := authz.Principal{
				User:        authz.User{Role: archetype.Role},
				Kind:        archetype.Kind,
				AllProjects: archetype.Global,
			}.Delegated(goldenTokenScopes(t, archetype.Scopes), archetype.CreatorRole)
			admitted := req.Public || req.Admits(p) == nil
			if admitted != (expected[i] == 'Y') {
				t.Errorf("%s: %s admitted=%t, golden=%c", pattern, archetype.Name, admitted, expected[i])
			}
		}
	}
}

func TestOperatorViewsRequireReadScope(t *testing.T) {
	requirements, err := ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	caller := authz.Principal{Kind: "machine", AllProjects: true}.Delegated([]authz.Operation{authz.Configure}, authz.RoleOwner)
	for _, pattern := range []string{
		"GET /api/v1/a2a-tasks",
		"GET /api/v1/a2a-tasks/{task_id}",
		"GET /api/v1/agent-delegations",
		"GET /api/v1/agent-runs",
		"GET /api/v1/agent-runs/{run_id}",
		"GET /api/v1/agent-runs/{run_id}/spans",
	} {
		requirement, ok := requirements[pattern]
		if !ok || requirement.Public || requirement.Admits(caller) == nil {
			t.Errorf("%s admitted a token without read scope", pattern)
		}
	}
}
