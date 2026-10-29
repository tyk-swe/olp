package access

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
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
			p := Principal{Kind: archetype.Kind, AllProjects: archetype.Global, creatorRole: archetype.CreatorRole}
			p.Role = archetype.Role
			for _, name := range archetype.Scopes {
				op, ok := ParseOperation(name)
				if !ok {
					t.Fatalf("%s has unknown scope %q", archetype.Name, name)
				}
				p.scopes |= 1 << op
			}
			admitted := req.Public || req.Admits(p) == nil
			if admitted != (expected[i] == 'Y') {
				t.Errorf("%s: %s admitted=%t, golden=%c", pattern, archetype.Name, admitted, expected[i])
			}
		}
	}
}

func TestRunManagementRequiresReadScope(t *testing.T) {
	requirements, err := ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	caller := Principal{Kind: "machine", AllProjects: true, creatorRole: RoleOwner, scopes: 1 << Configure}
	for _, pattern := range []string{
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
