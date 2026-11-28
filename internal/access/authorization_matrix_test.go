package access

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/keyauthority"
)

type authorizationGolden struct {
	Archetypes []authorizationArchetype `json:"archetypes"`
	Routes     map[string]string        `json:"routes"`
}

type authorizationArchetype struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Role        string   `json:"role"`
	CreatorRole string   `json:"creator_role"`
	Global      bool     `json:"global"`
	Scopes      []string `json:"scopes"`
}

func readAuthorizationGolden(t *testing.T) authorizationGolden {
	t.Helper()
	raw, err := os.ReadFile("testdata/authorization.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden authorizationGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	return golden
}

func (a authorizationArchetype) principal(t *testing.T) authz.Principal {
	t.Helper()
	if a.Kind == "key" {
		return authz.Principal{
			Kind: a.Kind,
			KeyAuthority: &keyauthority.Authority{
				Policy: keyauthority.KeyPolicy{Scopes: a.Scopes},
			},
		}
	}
	return authz.Principal{
		User:        authz.User{Role: a.Role},
		Kind:        a.Kind,
		AllProjects: a.Global,
	}.Delegated(goldenTokenScopes(t, a.Scopes), a.CreatorRole)
}

func goldenTokenScopes(t *testing.T, names []string) []authz.Operation {
	t.Helper()
	scopes := make([]authz.Operation, len(names))
	for i, name := range names {
		op, ok := authz.ParseOperation(name)
		if !ok {
			t.Fatalf("unknown token scope %q", name)
		}
		scopes[i] = op
	}
	return scopes
}

func TestManagementAuthorizationGoldenMatrix(t *testing.T) {
	golden := readAuthorizationGolden(t)
	requirements, err := ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	for pattern := range golden.Routes {
		if _, ok := requirements[pattern]; !ok {
			t.Errorf("%s has an authorization golden but is absent from the contract", pattern)
		}
	}
	for pattern, req := range requirements {
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
			p := archetype.principal(t)
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
