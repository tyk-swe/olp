package access

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestTheContractDeclaresEveryManagementRequirement(t *testing.T) {
	requirements, err := ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	if len(requirements) < 140 {
		t.Fatalf("the contract declares only %d management operations", len(requirements))
	}
	for pattern, req := range requirements {
		if !req.Public && len(req.Alternatives) == 0 {
			t.Errorf("%s admits nobody", pattern)
		}
	}
}

func TestContractRequirementsMustReadTruthfully(t *testing.T) {
	operation := func(path, method, security string) string {
		return `{"paths":{"` + path + `":{"` + method + `":{"security":` + security + `}}}}`
	}
	for name, document := range map[string]string{
		"missing security":        `{"paths":{"/api/v1/x":{"get":{}}}}`,
		"unknown scheme":          operation("/api/v1/x", "get", `[{"bearerToken":[]}]`),
		"unknown operation":       operation("/api/v1/x", "get", `[{"sessionCookie":["root"]}]`),
		"safe method with proof":  operation("/api/v1/x", "get", `[{"sessionCookie":["read"],"csrfToken":[]}]`),
		"unsafe method no proof":  operation("/api/v1/x", "post", `[{"sessionCookie":["configure"]}]`),
		"undelegable token scope": operation("/api/v1/x", "get", `[{"managementToken":["self"]}]`),
		"hidden installation":     operation("/api/v1/x", "get", `[{"sessionCookie":["settings"]}]`),
		"no operation":            operation("/api/v1/x", "get", `[{"sessionCookie":["installation"]}]`),
		"combined token":          operation("/api/v1/x", "get", `[{"managementToken":["read"],"sessionCookie":["read"]}]`),
	} {
		if _, err := parseRequirements([]byte(document)); err == nil {
			t.Errorf("%s: an untruthful contract loaded", name)
		}
	}
	valid := operation("/api/v1/x", "post", `[{"sessionCookie":["settings","installation"],"csrfToken":[]},{"managementToken":["settings","installation"]}]`)
	requirements, err := parseRequirements([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	req := requirements["POST /api/v1/x"]
	global := Principal{Kind: "user", User: User{Role: RoleOperator}, AllProjects: true}
	assigned := Principal{Kind: "user", User: User{Role: RoleOperator}}
	if req.Admits(global) != nil || req.Admits(assigned) == nil {
		t.Fatal("an installation requirement admitted the wrong members")
	}
	if public, err := parseRequirements([]byte(operation("/api/v1/x", "post", `[]`))); err != nil || !public["POST /api/v1/x"].Public {
		t.Fatal("an empty requirement must be public", err)
	}
}

// archetype is a caller the route-level golden evaluates, described so the
// console's own authorization can evaluate the same callers.
type archetype struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Role        string   `json:"role,omitempty"`
	CreatorRole string   `json:"creator_role,omitempty"`
	Global      bool     `json:"global"`
	Scopes      []string `json:"scopes,omitempty"`
}

func (a archetype) principal() Principal {
	p := Principal{Kind: a.Kind, User: User{Role: a.Role}, AllProjects: a.Global, creatorRole: a.CreatorRole}
	for _, name := range a.Scopes {
		op, _ := ParseOperation(name)
		p.scopes |= 1 << op
	}
	return p
}

func archetypes() []archetype {
	var all []archetype
	for _, role := range []string{RoleOwner, RoleOperator, RoleDeveloper, RoleViewer} {
		all = append(all,
			archetype{Name: role + " global", Kind: "user", Role: role, Global: true},
			archetype{Name: role + " assigned", Kind: "user", Role: role})
	}
	var every []string
	for _, op := range TokenScopes() {
		every = append(every, op.String())
	}
	return append(all,
		archetype{Name: "token every scope, owner global", Kind: "machine", CreatorRole: RoleOwner, Global: true, Scopes: every},
		archetype{Name: "token every scope, owner assigned", Kind: "machine", CreatorRole: RoleOwner, Scopes: every},
		archetype{Name: "token every scope, operator global", Kind: "machine", CreatorRole: RoleOperator, Global: true, Scopes: every},
		archetype{Name: "token read+configure, owner global", Kind: "machine", CreatorRole: RoleOwner, Global: true, Scopes: []string{"read", "configure"}},
	)
}

// TestRouteAuthorizationMatrix pins which callers every management route
// admits, derived from the contract and the policy together. Review a diff as
// a change of who may call the API.
func TestRouteAuthorizationMatrix(t *testing.T) {
	requirements, err := ContractRequirements()
	if err != nil {
		t.Fatal(err)
	}
	callers := archetypes()
	routes := map[string]string{}
	for pattern, req := range requirements {
		var row strings.Builder
		for _, caller := range callers {
			if req.Public || req.Admits(caller.principal()) == nil {
				row.WriteByte('Y')
			} else {
				row.WriteByte('-')
			}
		}
		routes[pattern] = row.String()
	}
	patterns := slices.Sorted(maps.Keys(routes))
	var out bytes.Buffer
	out.WriteString("{\n  \"archetypes\": [\n")
	for i, caller := range callers {
		encoded, _ := json.Marshal(caller)
		out.WriteString("    " + string(encoded))
		if i < len(callers)-1 {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	out.WriteString("  ],\n  \"routes\": {\n")
	for i, pattern := range patterns {
		fmt.Fprintf(&out, "    %q: %q", pattern, routes[pattern])
		if i < len(patterns)-1 {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	out.WriteString("  }\n}\n")
	golden(t, "authorization.golden.json", out.Bytes())
}

func TestHandlersMayAuthorizeOnlyDeclaredOperations(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/providers", nil)
	r.Pattern = "GET /api/v1/providers"
	checkDeclared(r, Read)
	defer func() {
		if recover() == nil {
			t.Fatal("an undeclared operation was not reported")
		}
	}()
	checkDeclared(r, Settings)
}
